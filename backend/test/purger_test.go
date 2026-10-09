package test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/lifecycle"
)

// memObjectStore records DeletePrefix calls and can be made to fail, so the
// purger's behaviour on a partial failure is testable without Garage.
type memObjectStore struct {
	mu      sync.Mutex
	deleted map[string]int // prefix -> keys removed
	calls   []string
	failOn  map[string]error
}

func newMemObjectStore() *memObjectStore {
	return &memObjectStore{
		deleted: map[string]int{},
		failOn:  map[string]error{},
	}
}

func (m *memObjectStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, prefix)
	if err, ok := m.failOn[prefix]; ok {
		return 0, err
	}
	m.deleted[prefix] = 3 // three files in every fixture site
	return 3, nil
}

func (m *memObjectStore) snapshot() ([]string, map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	calls := append([]string(nil), m.calls...)
	del := make(map[string]int, len(m.deleted))
	for k, v := range m.deleted {
		del[k] = v
	}
	return calls, del
}

// ageUpdatedAt backdates updated_at.
//
// The purger selects on updated_at, not created_at: MarkFailed resets it, so
// a row that failed a moment ago has a recent updated_at however old its
// creation is.
func ageUpdatedAt(t *testing.T, db *database.Database, id uint, d time.Duration) {
	t.Helper()

	res := db.DB().Exec(
		"UPDATE deployments SET updated_at = now() - make_interval(secs => $1::float8) WHERE id = $2",
		d.Seconds(), id,
	)
	if res.Error != nil {
		t.Fatalf("age deployment %d: %v", id, res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("expected to age 1 row, aged %d", res.RowsAffected)
	}
}

// TestPurgeRemovesObjectsThenRow is the core behaviour: a stale failed
// deployment loses both its objects and its row.
func TestPurgeRemovesObjectsThenRow(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if err := db.MarkFailed(ctx, res.Deployment.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	ageUpdatedAt(t, db, res.Deployment.ID, 2*lifecycle.Grace)

	store := newMemObjectStore()
	out, err := lifecycle.New(db, store).Once(ctx)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if out.Purged != 1 {
		t.Fatalf("Purged = %d, want 1", out.Purged)
	}
	if out.Objects != 3 {
		t.Errorf("Objects = %d, want 3", out.Objects)
	}

	// The objects went, under the right prefix.
	calls, del := store.snapshot()
	if len(calls) != 1 {
		t.Fatalf("DeletePrefix called %d times, want 1: %v", len(calls), calls)
	}
	if want := res.Deployment.Prefix + "/"; calls[0] != want {
		t.Errorf("deleted prefix %q, want %q", calls[0], want)
	}
	if del[calls[0]] != 3 {
		t.Errorf("recorded %d deleted under %q", del[calls[0]], calls[0])
	}

	// And so did the row.
	if _, err := db.GetDeployment(ctx, res.Deployment.ID); err != database.ErrNotFound {
		t.Errorf("the row should be gone, got %v", err)
	}
}

// TestPurgeRespectsGrace is the whole point of the two-stage design: a row
// that only just failed must keep its objects.
func TestPurgeRespectsGrace(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	recent := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if err := db.MarkFailed(ctx, recent.Deployment.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	// Failed, but only minutes ago.

	store := newMemObjectStore()
	out, err := lifecycle.New(db, store).Once(ctx)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if out.Purged != 0 {
		t.Fatalf("Purged = %d, want 0: a recently failed deployment must survive", out.Purged)
	}

	if calls, _ := store.snapshot(); len(calls) != 0 {
		t.Errorf("DeletePrefix was called for a deployment inside its grace: %v", calls)
	}
	if _, err := db.GetDeployment(ctx, recent.Deployment.ID); err != nil {
		t.Errorf("the row should still exist: %v", err)
	}
}

// TestPurgeNeverTouchesLiveOrClaimedDeployments is the property that protects
// real sites.
func TestPurgeNeverTouchesLiveOrClaimedDeployments(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	live := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	claimed := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if _, err := db.ClaimPermanently(ctx, claimed.ClaimToken, "u"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	owned := mustCreate(t, db, createOwned(uniquePrefix(t)))

	// Age all of them far beyond both graces, so only the guards protect them.
	for _, id := range []uint{live.Deployment.ID, claimed.Deployment.ID, owned.Deployment.ID} {
		ageRow(t, db, id, 100*lifecycle.Grace)
		ageUpdatedAt(t, db, id, 100*lifecycle.Grace)
	}

	store := newMemObjectStore()
	p := lifecycle.New(db, store)
	for range 3 {
		if _, err := p.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}

	if calls, _ := store.snapshot(); len(calls) != 0 {
		t.Errorf("DeletePrefix was called %d times for permanent deployments: %v", len(calls), calls)
	}
	for _, tc := range []struct {
		name string
		id   uint
	}{
		{"live", live.Deployment.ID},
		{"claimed", claimed.Deployment.ID},
		{"owned", owned.Deployment.ID},
	} {
		if _, err := db.GetDeployment(ctx, tc.id); err != nil {
			t.Errorf("the purger destroyed a %s deployment", tc.name)
		}
	}
}

// TestPurgeKeepsTheRowWhenObjectsCannotBeDeleted: a failed delete must leave
// something findable rather than a row pointing at nothing.
func TestPurgeKeepsTheRowWhenObjectsCannotBeDeleted(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if err := db.MarkFailed(ctx, res.Deployment.ID); err != nil {
		t.Fatal(err)
	}
	ageUpdatedAt(t, db, res.Deployment.ID, 2*lifecycle.Grace)

	store := newMemObjectStore()
	store.failOn[res.Deployment.Prefix+"/"] = errStorageDown

	out, err := lifecycle.New(db, store).Once(ctx)
	if err != nil {
		t.Fatalf("a storage failure must not fail the pass: %v", err)
	}
	if out.Purged != 0 {
		t.Errorf("Purged = %d, want 0 when the delete fails", out.Purged)
	}
	if out.Failed != 1 {
		t.Errorf("Failed = %d, want 1", out.Failed)
	}

	// The row is still there to retry against.
	got, err := db.GetDeployment(ctx, res.Deployment.ID)
	if err != nil {
		t.Fatalf("the row must survive a failed delete: %v", err)
	}
	if got.Status != database.StatusPurging {
		t.Errorf("Status = %q, want purging so the retry is visible", got.Status)
	}
}

var errStorageDown = &storageError{}

type storageError struct{}

func (*storageError) Error() string { return "storage unavailable" }

// TestPurgeDeletesObjectsBeforeTheRow pins the ordering, which is not
// observable from the result but is the whole reason a crash mid-purge is
// recoverable.
func TestPurgeDeletesObjectsBeforeTheRow(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if err := db.MarkFailed(ctx, res.Deployment.ID); err != nil {
		t.Fatal(err)
	}
	ageUpdatedAt(t, db, res.Deployment.ID, 2*lifecycle.Grace)

	store := newMemObjectStore()
	// Fail the delete, so the row survives and we can observe that the store
	// was called before the row was touched.
	store.failOn[res.Deployment.Prefix+"/"] = errStorageDown

	if _, err := lifecycle.New(db, store).Once(ctx); err != nil {
		t.Fatal(err)
	}

	calls, _ := store.snapshot()
	if len(calls) == 0 {
		t.Fatal("the object store was never called")
	}
	if _, err := db.GetDeployment(ctx, res.Deployment.ID); err != nil {
		t.Errorf("the row was removed before its objects: %v", err)
	}
}

// TestSweepRemovesExpiredUnclaimedDrops is the expiry path: a window that
// closed and was never claimed loses its objects and its row.
func TestSweepRemovesExpiredUnclaimedDrops(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	drop := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	expireNow(t, db, drop.Deployment.Prefix)

	store := newMemObjectStore()
	out, err := lifecycle.NewSweeper(db, store).Once(ctx)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if out.Purged != 1 {
		t.Fatalf("Swept = %d, want 1", out.Purged)
	}

	calls, _ := store.snapshot()
	if len(calls) != 1 || calls[0] != drop.Deployment.Prefix+"/" {
		t.Errorf("deleted %v, want %q", calls, drop.Deployment.Prefix+"/")
	}
	if _, err := db.GetDeployment(ctx, drop.Deployment.ID); err != database.ErrNotFound {
		t.Errorf("the row should be gone, got %v", err)
	}
}

// TestSweepNeverTouchesAPendingUpload is the dangerous case. A pending row has
// an expiry set, because CreateDeployment applies the TTL before the upload
// runs. Keying on expiry alone would let a slow upload be destroyed while it
// was still being written.
func TestSweepNeverTouchesAPendingUpload(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	// Created with a TTL but never promoted: an upload still in flight.
	inflight := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if inflight.Deployment.Status != database.StatusPending {
		t.Fatalf("precondition: status is %q, want pending", inflight.Deployment.Status)
	}
	if inflight.Deployment.ExpiresAt == nil {
		t.Fatal("precondition: a pending row must carry an expiry, which is the trap")
	}
	// And it is past that expiry.
	expireNow(t, db, inflight.Deployment.Prefix)

	store := newMemObjectStore()
	out, err := lifecycle.NewSweeper(db, store).Once(ctx)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if out.Purged != 0 {
		t.Fatalf("an in-flight upload was swept, Purged = %d", out.Purged)
	}
	if calls, _ := store.snapshot(); len(calls) != 0 {
		t.Errorf("objects were deleted for an in-flight upload: %v", calls)
	}
	if _, err := db.GetDeployment(ctx, inflight.Deployment.ID); err != nil {
		t.Errorf("the row must survive: %v", err)
	}
}

// TestSweepNeverTouchesPermanentDeployments covers claimed and owned rows,
// which have no expiry at all.
func TestSweepNeverTouchesPermanentDeployments(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	claimed := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	if _, err := db.ClaimPermanently(ctx, claimed.ClaimToken, "u"); err != nil {
		t.Fatal(err)
	}
	owned := mustCreate(t, db, createOwned(uniquePrefix(t)))

	// Age both far past anything, so only the guards protect them.
	ageRow(t, db, claimed.Deployment.ID, 100*lifecycle.Grace)
	ageUpdatedAt(t, db, claimed.Deployment.ID, 100*lifecycle.Grace)
	ageRow(t, db, owned.Deployment.ID, 100*lifecycle.Grace)
	ageUpdatedAt(t, db, owned.Deployment.ID, 100*lifecycle.Grace)

	store := newMemObjectStore()
	s := lifecycle.NewSweeper(db, store)
	for range 3 {
		out, err := s.Once(ctx)
		if err != nil {
			t.Fatalf("Once: %v", err)
		}
		if out.Purged != 0 {
			t.Fatalf("swept %d permanent deployments", out.Purged)
		}
	}
	if calls, _ := store.snapshot(); len(calls) != 0 {
		t.Errorf("objects deleted for permanent deployments: %v", calls)
	}
}

// TestSweepRetriesAnInterruptedDelete: a deployment left mid-sweep must be
// picked up again rather than stranding its objects.
func TestSweepRetriesAnInterruptedDelete(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	drop := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour,
	})
	expireNow(t, db, drop.Deployment.Prefix)

	// First pass claims it, then the delete fails.
	failing := newMemObjectStore()
	failing.failOn[drop.Deployment.Prefix+"/"] = errStorageDown
	out, err := lifecycle.NewSweeper(db, failing).Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Purged != 0 || out.Failed != 1 {
		t.Fatalf("first pass: Purged=%d Failed=%d, want 0 and 1", out.Purged, out.Failed)
	}

	got, err := db.GetDeployment(ctx, drop.Deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != database.StatusPurging {
		t.Fatalf("Status = %q, want purging so the retry is visible", got.Status)
	}

	// Second pass, storage healthy, must finish the job.
	healthy := newMemObjectStore()
	out, err = lifecycle.NewSweeper(db, healthy).Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Purged != 1 {
		t.Errorf("second pass Purged = %d, want 1", out.Purged)
	}
	if _, err := db.GetDeployment(ctx, drop.Deployment.ID); err != database.ErrNotFound {
		t.Errorf("the row should be gone after the retry, got %v", err)
	}
}
