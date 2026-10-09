package test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/reconciler"
	"github.com/dk10ws/ironhide/internal/upload"
)

// ageRow backdates created_at so a row looks older than it is, standing in
// for time passing without the test having to sleep.
func ageRow(t *testing.T, db *database.Database, id uint, d time.Duration) {
	t.Helper()

	// make_interval takes a number of seconds. Casting a text parameter to
	// interval is inferred as bigint by Postgres and fails.
	res := db.DB().Exec(
		"UPDATE deployments SET created_at = now() - make_interval(secs => $1::float8) WHERE id = $2",
		d.Seconds(), id,
	)
	if res.Error != nil {
		t.Fatalf("age deployment %d: %v", id, res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("expected to age 1 row, aged %d", res.RowsAffected)
	}
}

// TestDeploymentStartsPending is the ordering the whole design rests on: the
// row is written before the objects, so a failed upload leaves a findable row
// instead of bytes nothing describes.
func TestDeploymentStartsPending(t *testing.T) {
	db := liveDB(t)

	// No Files: the handler creates the row before the objects exist, so the
	// counts are still zero and RecordUpload fills them in.
	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})

	if res.Deployment.Status != database.StatusPending {
		t.Errorf("Status = %q, want pending", res.Deployment.Status)
	}
	if res.Deployment.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0 before the objects arrive", res.Deployment.FileCount)
	}
	if len(res.Deployment.Manifest) != 0 {
		t.Errorf("manifest = %+v, want empty before the objects arrive", res.Deployment.Manifest)
	}

	// A pending drop is not claimable: its objects may not all exist yet.
	if _, err := db.ClaimPermanently(context.Background(), res.ClaimToken, "u"); err != database.ErrAlreadyClaimed {
		t.Errorf("a pending drop must not be claimable, got %v", err)
	}
}

// TestRecordUploadPromotesToLive covers the second step.
func TestRecordUploadPromotesToLive(t *testing.T) {
	db := liveDB(t)

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), Files: manifest(), TTL: time.Hour,
	})

	// The stored prefix has no trailing slash, while object keys do.
	base := res.Deployment.Prefix + "/"
	outcome := upload.Result{
		Prefix: base,
		Bytes:  27,
		Files: []upload.File{
			{Key: base + "index.html", Size: 13, ETag: "aaa"},
			{Key: base + "assets/app.js", Size: 14, ETag: "bbb"},
		},
		HasIndexHTML: true,
	}

	if err := db.RecordUpload(context.Background(), res.Deployment.ID, outcome); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}

	loaded, err := db.GetDeployment(context.Background(), res.Deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != database.StatusLive {
		t.Errorf("Status = %q, want live", loaded.Status)
	}
	if loaded.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", loaded.FileCount)
	}
	if loaded.Bytes != 27 {
		t.Errorf("Bytes = %d, want 27", loaded.Bytes)
	}
	if len(loaded.Manifest) != 2 {
		t.Errorf("manifest = %+v, want 2 entries", loaded.Manifest)
	}
	// The index must be resolvable from the manifest, or the site cannot serve.
	if !hasIndex(loaded) {
		t.Error("a live deployment must have an index.html in its manifest")
	}

	// And it is now claimable.
	if _, err := db.ClaimPermanently(context.Background(), res.ClaimToken, "u"); err != nil {
		t.Errorf("a live drop must be claimable, got %v", err)
	}
}

func hasIndex(d *database.Deployment) bool {
	for _, e := range d.Manifest {
		if e.Key == d.Prefix+"/index.html" {
			return true
		}
	}
	return false
}

// TestRecordUploadRejectsAFailedRow stops a reconciler and a handler racing:
// once a row is failed, a late completion must not promote it.
func TestRecordUploadRejectsAFailedRow(t *testing.T) {
	db := liveDB(t)

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), Files: manifest(), TTL: time.Hour,
	})
	if err := db.MarkFailed(context.Background(), res.Deployment.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	err := db.RecordUpload(context.Background(), res.Deployment.ID, upload.Result{Bytes: 27})
	if err == nil {
		t.Fatal("a failed deployment must not be promoted back to live")
	}
}

// TestReconcilerAbandonsStuckUploads is the payoff of writing the row first.
func TestReconcilerAbandonsStuckUploads(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	stuck := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	fresh := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	live := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), Files: manifest(), TTL: time.Hour,
	})

	// Only the abandoned one is old enough.
	ageRow(t, db, stuck.Deployment.ID, 2*reconciler.Grace)

	res, err := reconciler.New(db).Once(ctx)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Abandoned != 1 {
		t.Fatalf("Abandoned = %d, want 1", res.Abandoned)
	}

	for _, tc := range []struct {
		name string
		id   uint
		want string
	}{
		{"stuck pending", stuck.Deployment.ID, database.StatusFailed},
		{"fresh pending", fresh.Deployment.ID, database.StatusPending},
		{"live", live.Deployment.ID, database.StatusLive},
	} {
		got, err := db.GetDeployment(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.want {
			t.Errorf("%s: Status = %q, want %q", tc.name, got.Status, tc.want)
		}
	}
}

// TestReconcilerNeverTouchesClaimedOrLiveDeployments is the property that
// matters most: a permanent site must survive any number of passes.
func TestReconcilerNeverTouchesClaimedOrLiveDeployments(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	claimed := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), Files: manifest(), TTL: time.Hour,
	})
	if _, err := db.ClaimPermanently(ctx, claimed.ClaimToken, "u"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	owned := mustCreate(t, db, createOwned(uniquePrefix(t)))

	// Age both far beyond the grace period, so only the status guards them.
	ageRow(t, db, claimed.Deployment.ID, 100*reconciler.Grace)
	ageRow(t, db, owned.Deployment.ID, 100*reconciler.Grace)

	r := reconciler.New(db)
	for pass := range 3 {
		res, err := r.Once(ctx)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if res.Abandoned != 0 {
			t.Errorf("pass %d abandoned %d deployments, want 0", pass, res.Abandoned)
		}
	}

	for _, tc := range []struct {
		name string
		id   uint
	}{
		{"claimed", claimed.Deployment.ID},
		{"owned", owned.Deployment.ID},
	} {
		got, err := db.GetDeployment(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == database.StatusFailed {
			t.Errorf("the reconciler failed a %s deployment", tc.name)
		}
		if got.ExpiresAt != nil {
			t.Errorf("%s deployment gained an expiry", tc.name)
		}
	}
}

// TestReconcilerDoesNotRaceAnUploadInProgress guards the dangerous case: a
// fresh pending row is an upload still running, not a stuck one.
func TestReconcilerDoesNotRaceAnUploadInProgress(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	inflight := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})

	res, err := reconciler.New(db).Once(ctx)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if res.Abandoned != 0 {
		t.Fatalf("a just-created pending row must not be abandoned, abandoned %d", res.Abandoned)
	}

	got, err := db.GetDeployment(ctx, inflight.Deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != database.StatusPending {
		t.Errorf("Status = %q, want pending", got.Status)
	}
}

// TestRejectedUploadsLeaveNoRow is what keeps the deployments table from
// being fillable for free. Every rejection is an unauthenticated request, and
// almost all of them are validation failures that never touch the bucket, so
// there is nothing for a row to describe.
func TestRejectedUploadsLeaveNoRow(t *testing.T) {
	srv, db, _ := newTestServer(t)

	count := func() int64 {
		var n int64
		db.DB().Raw("SELECT count(*) FROM deployments").Row().Scan(&n)
		return n
	}

	cases := []struct {
		name string
		body func(t *testing.T) ([]byte, string)
	}{
		{"traversal filename", func(t *testing.T) ([]byte, string) {
			return multipartBody(t, "../../../etc/passwd", "x")
		}},
		{"no index.html", func(t *testing.T) ([]byte, string) {
			return multipartBody(t, "readme.txt", "x")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := count()
			body, ct := tc.body(t)
			resp := doMultipart(t, srv, controlHost, "/api/upload/files", body, ct)
			if resp.StatusCode == http.StatusCreated {
				t.Fatalf("expected a rejection, got 201")
			}
			if after := count(); after != before {
				t.Errorf("a rejected upload left %d row(s) behind", after-before)
			}
		})
	}
}
