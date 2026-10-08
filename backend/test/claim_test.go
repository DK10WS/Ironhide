package test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
)

func liveDB(t *testing.T) *database.Database {
	t.Helper()

	host := os.Getenv("TEST_POSTGRES_HOST")
	if host == "" {
		t.Skip("set TEST_POSTGRES_HOST to run database tests")
	}

	db, err := database.Open(context.Background(), config.Postgres{
		Host:     host,
		Port:     5432,
		User:     os.Getenv("TEST_POSTGRES_USER"),
		Password: os.Getenv("TEST_POSTGRES_PASSWORD"),
		Database: os.Getenv("TEST_POSTGRES_DB"),
		SSLMode:  "disable",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func manifest() []database.ManifestEntry {
	return []database.ManifestEntry{
		{Key: "uploads/abc/index.html", Size: 13, ETag: "aaa"},
		{Key: "uploads/abc/assets/app.js", Size: 14, ETag: "bbb"},
	}
}

func TestCreateDeploymentFillsEveryColumn(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	res, err := db.CreateDeployment(ctx, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t),
		Files:  manifest(),
		TTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	cleanupDeployments(t, db, res.Deployment.Prefix)
	d := res.Deployment

	if d.ID == 0 {
		t.Error("ID not assigned")
	}
	if d.SiteID != nil {
		t.Error("an anonymous drop must have a nil SiteID")
	}
	// The trailing slash is stripped so PublicURLPath cannot double up.
	if strings.HasPrefix(d.Prefix, "uploads/") == false {
		t.Errorf("Prefix = %q", d.Prefix)
	}
	if d.Status != database.StatusLive {
		t.Errorf("Status = %q, want live", d.Status)
	}
	if d.FileCount != 2 || d.Bytes != 27 {
		t.Errorf("FileCount = %d, Bytes = %d, want 2 and 27", d.FileCount, d.Bytes)
	}
	if d.ExpiresAt == nil {
		t.Error("ExpiresAt must be set for a claimable drop")
	}
	if d.Claimed() || !d.Anonymous() {
		t.Error("a fresh drop is unclaimed and anonymous")
	}
	// The trailing slash is stripped on write so the URL and the object keys
	// cannot drift apart into "/uploads/<id>//index.html".
	if !strings.HasPrefix(d.Prefix, "uploads/") || strings.HasSuffix(d.Prefix, "/") {
		t.Errorf("Prefix = %q, want uploads/<id> with no trailing slash", d.Prefix)
	}
	if strings.Contains(d.PublicURLPath(), "//") {
		t.Errorf("PublicURLPath = %q, must not contain a doubled slash", d.PublicURLPath())
	}

	// The token is returned once and only its hash is kept.
	if res.ClaimToken == "" {
		t.Error("ClaimToken must be returned")
	}
	if d.ClaimTokenHash != database.HashClaimToken(res.ClaimToken) {
		t.Error("stored hash does not match the returned token")
	}
	if d.ClaimTokenHash == res.ClaimToken {
		t.Error("the raw token must never be stored")
	}

	// Manifest must survive the jsonb round trip.
	loaded, err := db.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if len(loaded.Manifest) != 2 {
		t.Fatalf("manifest lost: %+v", loaded.Manifest)
	}
	if loaded.Manifest[0].Key != "uploads/abc/index.html" || loaded.Manifest[0].ETag != "aaa" {
		t.Errorf("manifest entry corrupted: %+v", loaded.Manifest[0])
	}
}

func TestOwnedDeploymentNeverExpires(t *testing.T) {
	db := liveDB(t)

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix:    uniquePrefix(t),
		Files:     manifest(),
		ClaimedBy: "user-42",
	})

	d := res.Deployment
	if !d.Claimed() {
		t.Error("an owned deployment is claimed immediately")
	}
	if d.ExpiresAt != nil {
		t.Error("an owned deployment must have no expiry, stored as NULL")
	}
	if d.Expired(time.Now()) {
		t.Error("a zero ExpiresAt must never read as expired")
	}
	if d.ClaimTokenHash != "" {
		t.Error("an owned deployment needs no claim token")
	}
}

func TestCreateRejectsEmptyPrefix(t *testing.T) {
	db := liveDB(t)

	for _, prefix := range []string{"", "/", "///"} {
		_, err := db.CreateDeployment(context.Background(), database.CreateDeploymentInput{
			Prefix: prefix,
			Files:  manifest(),
			TTL:    time.Hour,
		})
		if err == nil {
			t.Errorf("prefix %q should be rejected", prefix)
		}
	}
}

// TestClaimIsSingleUse is the security-relevant test: the token is the only
// credential for an unclaimed drop, so redeeming it twice must be impossible.
//
// The single-use property comes from a conditional UPDATE on claimed_at IS
// NULL, decided by the row lock rather than a read-then-write race. Run with
// -race to exercise it properly.
func TestClaimIsSingleUse(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	const racers = 16

	created := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t),
		Files:  manifest(),
		TTL:    time.Hour,
	})

	var (
		mu      sync.Mutex
		winner  *database.Deployment
		winners int
		losers  int
		start   = make(chan struct{})
		wg      sync.WaitGroup
	)

	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			claimed, err := db.ClaimPermanently(ctx, created.ClaimToken, "racer")
			mu.Lock()
			defer mu.Unlock()
			switch err {
			case nil:
				winners++
				winner = claimed
			case database.ErrAlreadyClaimed:
				losers++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("exactly one winner expected, got %d", winners)
	}
	if losers != racers-1 {
		t.Errorf("expected %d losers, got %d", racers-1, losers)
	}

	// The claimed row comes back from RETURNING, so the caller does not need
	// the spent hash to find it again.
	if winner == nil {
		t.Fatal("no deployment was returned by the winning claim")
	}
	claimed := winner
	if claimed.ID == 0 {
		t.Error("ClaimPermanently must return the claimed deployment")
	}
	if !claimed.Claimed() {
		t.Error("the returned deployment should be claimed")
	}
	if claimed.ExpiresAt != nil {
		t.Error("a claimed deployment must have no expiry, stored as NULL")
	}
	if claimed.Expired(time.Now()) {
		t.Error("a claimed deployment must never read as expired")
	}

	reloaded, err := db.GetDeployment(ctx, created.Deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ClaimTokenHash != "" {
		t.Error("a spent token hash must be cleared")
	}
	if reloaded.ExpiresAt != nil {
		t.Error("claiming must clear expires_at to NULL")
	}
}

func TestClaimRejectsWrongToken(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	created, err := db.CreateDeployment(ctx, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t),
		Files:  manifest(),
		TTL:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupDeployments(t, db, created.Deployment.Prefix)

	// An empty token is rejected earlier than a well-formed wrong one, so it
	// reports ErrNotFound rather than ErrAlreadyClaimed. Both refuse, and
	// neither distinction tells an attacker anything useful: no token of any
	// shape ever redeems a drop it did not create.
	tokens := map[string]error{
		"":                       database.ErrNotFound,
		"nonsense":               database.ErrAlreadyClaimed,
		created.ClaimToken + "x": database.ErrAlreadyClaimed,
		created.ClaimToken[:len(created.ClaimToken)-1]: database.ErrAlreadyClaimed,
	}
	for token, want := range tokens {
		_, err := db.ClaimPermanently(ctx, token, "u")
		if err != want {
			t.Errorf("token %q: want %v, got %v", token, want, err)
		}
	}
}

// TestClaimRefusesExpiredDrop proves the claim window holds even if the
// sweeper has not run.
func TestClaimRefusesExpiredDrop(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	created, err := db.CreateDeployment(ctx, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t),
		Files:  manifest(),
		TTL:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupDeployments(t, db, created.Deployment.Prefix)
	expireNow(t, db, created.Deployment.Prefix)

	if _, err := db.ClaimPermanently(ctx, created.ClaimToken, "u"); err != database.ErrAlreadyClaimed {
		t.Errorf("an expired drop must not be claimable, got %v", err)
	}
}

// TestListExpiredSkipsEverythingPermanent is what stops the sweeper deleting a
// claimed site.
func TestListExpiredSkipsEverythingPermanent(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	stale, err := db.CreateDeployment(ctx, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t),
		Files:  manifest(),
		TTL:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	owned, err := db.CreateDeployment(ctx, database.CreateDeploymentInput{
		Prefix:    uniquePrefix(t),
		Files:     manifest(),
		ClaimedBy: "user-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupDeployments(t, db, stale.Deployment.Prefix, owned.Deployment.Prefix)
	expireNow(t, db, stale.Deployment.Prefix)

	expired, err := db.ListExpired(ctx, 100)
	if err != nil {
		t.Fatalf("ListExpired: %v", err)
	}

	found := false
	for _, d := range expired {
		if d.Prefix == owned.Deployment.Prefix {
			t.Error("the sweeper must never return an owned deployment")
		}
		if d.Claimed() {
			t.Errorf("the sweeper must never return a claimed deployment: %s", d.Prefix)
		}
		if d.Prefix == stale.Deployment.Prefix {
			found = true
		}
	}
	if !found {
		t.Error("the expired drop is missing from the work list")
	}
}

func TestUpsertSiteIsIdempotentOnDomain(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	domain := "upsert-" + t.Name() + ".example"

	first := mustSite(t, db, "slug-a", domain, "")
	second := mustSite(t, db, "slug-b", domain, "user-7")

	if first.ID != second.ID {
		t.Errorf("upsert created a new row: %d then %d", first.ID, second.ID)
	}
	if second.OwnerID != "user-7" {
		t.Errorf("OwnerID = %q, want user-7", second.OwnerID)
	}

	byDomain, err := db.GetSiteByDomain(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	if byDomain.ID != first.ID {
		t.Errorf("GetSiteByDomain = %d, want %d", byDomain.ID, first.ID)
	}
}

func TestGetDeploymentByPrefixNotFound(t *testing.T) {
	db := liveDB(t)

	_, err := db.GetDeploymentByPrefix(context.Background(), "uploads/does-not-exist-xyz")
	if err != database.ErrNotFound {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestCreateRejectsNegativeTTL pins a real defect: a negative TTL used to fall
// through the "TTL > 0" branch, leaving ExpiresAt zero, which reads as never
// expiring. The caller got a permanent deployment instead of an error, and
// nothing would ever have swept it.
func TestCreateRejectsNegativeTTL(t *testing.T) {
	db := liveDB(t)

	cases := map[string]database.CreateDeploymentInput{
		"anonymous drop": {
			Prefix: uniquePrefix(t),
			Files:  manifest(),
			TTL:    -time.Minute,
		},
		// The owned path short-circuits the TTL branch entirely, so this case
		// is checked separately: it must not be a way to smuggle a bad TTL past
		// validation either.
		"owned deployment": {
			Prefix:    uniquePrefix(t),
			Files:     manifest(),
			ClaimedBy: "user-1",
			TTL:       -time.Hour,
		},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := db.CreateDeployment(context.Background(), in)
			if err == nil {
				// If it was accepted, it is a permanent row: remove it.
				t.Cleanup(func() {
					db.DB().Exec("DELETE FROM deployments WHERE id = ?", res.Deployment.ID)
				})
				t.Fatalf("a negative TTL was accepted and produced a deployment "+
					"with ExpiresAt %v (nil means permanent)", res.Deployment.ExpiresAt)
			}
			if !errors.Is(err, database.ErrInvalidTTL) {
				t.Errorf("want ErrInvalidTTL, got %v", err)
			}
		})
	}
}

// TestZeroTTLMakesADropPermanent documents the other half: zero is a valid
// request meaning "no expiry", not an error.
func TestZeroTTLMakesADropPermanent(t *testing.T) {
	db := liveDB(t)

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t),
		Files:  manifest(),
		TTL:    0,
	})

	if res.Deployment.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want NULL for a permanent deployment",
			res.Deployment.ExpiresAt)
	}
	if res.Deployment.Expired(time.Now()) {
		t.Error("a deployment with no expiry must never read as expired")
	}

	// And it must not appear in the sweeper's work list.
	expired, err := db.ListExpired(context.Background(), 100)
	if err != nil {
		t.Fatalf("ListExpired: %v", err)
	}
	for _, d := range expired {
		if d.ID == res.Deployment.ID {
			t.Error("a deployment with no expiry must never be swept")
		}
	}
}

// TestClaimPermanentlyClearsExpiry is the property the sweeper depends on:
// claiming must turn a drop into a permanent deployment that no expiry query
// can select.
func TestClaimPermanentlyClearsExpiry(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	created := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), Files: manifest(), TTL: time.Hour,
	})
	if created.Deployment.ExpiresAt == nil {
		t.Fatal("precondition: a drop must have an expiry")
	}

	claimed, err := db.ClaimPermanently(ctx, created.ClaimToken, "claim-token")
	if err != nil {
		t.Fatalf("ClaimPermanently: %v", err)
	}
	if claimed.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want NULL after claiming", claimed.ExpiresAt)
	}
	if !claimed.Claimed() {
		t.Error("the deployment must report as claimed")
	}
	if claimed.Expired(time.Now()) {
		t.Error("a claimed deployment must never read as expired")
	}

	// And it must not appear in the sweeper's work list.
	expired, err := db.ListExpired(ctx, 100)
	if err != nil {
		t.Fatalf("ListExpired: %v", err)
	}
	for _, d := range expired {
		if d.ID == claimed.ID {
			t.Error("a claimed deployment must never be swept")
		}
	}
}

// TestClaimRefusesAnAlreadyPermanentDeployment: a deployment with no expiry was
// never a drop, so there is nothing to claim and no token to redeem.
func TestClaimRefusesAnAlreadyPermanentDeployment(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()

	owned := mustCreate(t, db, createOwned(uniquePrefix(t)))

	// The owned row has no token hash at all, so any token must fail.
	if _, err := db.ClaimPermanently(ctx, "any-token-at-all", "u"); err != database.ErrAlreadyClaimed {
		t.Errorf("want ErrAlreadyClaimed, got %v", err)
	}
	if owned.Deployment.ClaimTokenHash != "" {
		t.Error("an owned deployment should have no token hash")
	}
}
