package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/api"
	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
)

// authConfig is a control plane with Google credentials present, which is what
// decides whether the sign-in routes exist at all.
func authConfig() config.Config {
	c := testConfig()
	c.FrontendOrigin = "http://localhost:7210"
	c.Google = config.Google{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURI:  "http://localhost:7210/api/auth/google/callback",
	}
	c.Session = config.Session{Lifetime: time.Hour}
	c.Quota = config.Quota{BytesPerUser: 1 << 30}
	return c
}

func newAuthServer(t *testing.T, cfg config.Config) (*httptest.Server, *database.Database) {
	t.Helper()
	db := liveDB(t)

	srv := httptest.NewServer(api.NewRouter(cfg, newMemStore(), db))
	t.Cleanup(srv.Close)

	// Sign-in is a series of redirects, ending at Google. Following them would
	// make a correct 302 look like a 200 from Google, so these tests assert on
	// the redirect itself.
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return srv, db
}

// withCookie issues a request carrying one cookie, which is how a session
// reaches the handler. The plain helpers in this package cannot, since they
// only set a Host.
func withCookie(t *testing.T, srv *httptest.Server, method, path, name, value string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = controlHost
	req.AddCookie(&http.Cookie{Name: name, Value: value})

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// mustUser upserts an account and guarantees it is removed.
func mustUser(t *testing.T, db *database.Database, id string) *database.User {
	t.Helper()
	u, err := db.UpsertUser(context.Background(), database.User{
		ID: id, Email: id + "@example.com", EmailVerified: true, Name: "Someone",
	})
	if err != nil {
		t.Fatalf("UpsertUser %s: %v", id, err)
	}
	t.Cleanup(func() {
		if err := db.DB().Exec("DELETE FROM users WHERE id = ?", id).Error; err != nil {
			t.Logf("cleanup user %s: %v", id, err)
		}
		if err := db.DB().Exec("DELETE FROM sessions WHERE user_id = ?", id).Error; err != nil {
			t.Logf("cleanup sessions %s: %v", id, err)
		}
		// Sites carry the owner as a plain string, so nothing cascades. Left
		// behind they would count against the next test using the same id.
		if err := db.DB().Exec("DELETE FROM sites WHERE owner_id = ?", id).Error; err != nil {
			t.Logf("cleanup sites %s: %v", id, err)
		}
		// Quota is measured from live deployments, so a row left behind makes
		// the next test using this owner start already over its allowance.
		if err := db.DB().Exec("DELETE FROM deployments WHERE owner_id = ?", id).Error; err != nil {
			t.Logf("cleanup deployments %s: %v", id, err)
		}
	})
	return u
}

// TestAuthRoutesAbsentWithoutCredentials is the property that an installation
// with no Google keys offers no endpoints that look like sign-in but cannot
// work.
func TestAuthRoutesAbsentWithoutCredentials(t *testing.T) {
	srv, _ := newAuthServer(t, testConfig())

	for _, path := range []string{"/api/auth/google", "/api/auth/google/callback"} {
		if got := do(t, srv, "GET", controlHost, path, nil).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET %s without credentials = %d, want 404", path, got)
		}
	}
}

// TestAuthRoutesPresentWithCredentials is the other half: with keys they exist,
// and the redirect carries what Google needs.
func TestAuthRoutesPresentWithCredentials(t *testing.T) {
	srv, _ := newAuthServer(t, authConfig())

	resp := do(t, srv, "GET", controlHost, "/api/auth/google", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /api/auth/google = %d, want 302", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	for _, want := range []string{"client_id=test-client-id", "state=", "accounts.google.com"} {
		if !strings.Contains(loc, want) {
			t.Errorf("redirect %q missing %q", loc, want)
		}
	}

	// The state cookie is what makes the callback verifiable.
	var state string
	for _, c := range resp.Cookies() {
		if c.Name == "ironhide_oauth_state" {
			state = c.Value
		}
	}
	if state == "" {
		t.Fatal("no state cookie set, so the callback cannot verify the round trip")
	}
	if !strings.Contains(loc, "state="+state) {
		t.Error("the state in the redirect does not match the cookie")
	}
}

// TestCallbackRejectsMismatchedState is the CSRF property. A callback whose
// state does not match the cookie must not create a session, or an attacker
// could sign a victim into an account they control.
func TestCallbackRejectsMismatchedState(t *testing.T) {
	srv, db := newAuthServer(t, authConfig())

	resp := withCookie(t, srv, "GET",
		"/api/auth/google/callback?code=abc&state=wrong", "ironhide_oauth_state", "right")

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback with a bad state = %d, want a redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("redirect %q should carry an error, not sign anyone in", loc)
	}
	if got := countSessions(t, db); got != 0 {
		t.Errorf("a rejected callback created %d sessions", got)
	}
}

// TestCallbackWithoutStateCookieIsRejected covers the same property with no
// cookie at all.
func TestCallbackWithoutStateCookieIsRejected(t *testing.T) {
	srv, db := newAuthServer(t, authConfig())

	resp := do(t, srv, "GET", controlHost, "/api/auth/google/callback?code=abc&state=whatever", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback without a state cookie = %d, want a redirect", resp.StatusCode)
	}
	if got := countSessions(t, db); got != 0 {
		t.Errorf("a callback with no state cookie created %d sessions", got)
	}
}

// TestMeRequiresSession is the property every protected route depends on.
func TestMeRequiresSession(t *testing.T) {
	srv, _ := newAuthServer(t, authConfig())

	if got := do(t, srv, "GET", controlHost, "/api/me", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /api/me with no cookie = %d, want 401", got)
	}

	resp := withCookie(t, srv, "GET", "/api/me", "ironhide_session", "made-up")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/me with an unknown token = %d, want 401", resp.StatusCode)
	}
}

// TestMeWithLiveSession proves the cookie resolves to the account.
func TestMeWithLiveSession(t *testing.T) {
	srv, db := newAuthServer(t, authConfig())
	user := mustUser(t, db, "sub-me")

	token, _, err := db.NewSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	resp := withCookie(t, srv, "GET", "/api/me", "ironhide_session", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me with a live session = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "sub-me@example.com") {
		t.Errorf("body %q does not name the account", body)
	}
}

// TestSessionExpiry proves an expired cookie stops working and is cleaned up,
// which is what makes the lifetime meaningful rather than decorative.
func TestSessionExpiry(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()
	user := mustUser(t, db, "sub-expired")

	// A negative lifetime so the row is already past its expiry.
	token, _, err := db.NewSession(ctx, user.ID, -time.Minute)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if _, err := db.SessionUser(ctx, token, time.Now()); err == nil {
		t.Error("an expired session still resolved to a user")
	}

	var n int64
	if err := db.DB().Model(&database.Session{}).Where("user_id = ?", user.ID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("the expired session was left in the table (%d rows)", n)
	}
}

// TestLogoutRetiresTheSession is the property that signing out ends the session
// rather than only clearing the browser's copy.
func TestLogoutRetiresTheSession(t *testing.T) {
	srv, db := newAuthServer(t, authConfig())
	ctx := context.Background()
	user := mustUser(t, db, "sub-out")

	token, _, err := db.NewSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := db.SessionUser(ctx, token, time.Now()); err != nil {
		t.Fatalf("session should be live: %v", err)
	}

	if resp := withCookie(t, srv, "POST", "/api/auth/logout", "ironhide_session", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d, want 200", resp.StatusCode)
	}

	// The token must be dead server-side, not merely forgotten by the browser.
	if _, err := db.SessionUser(ctx, token, time.Now()); err == nil {
		t.Error("the session still works after signing out")
	}
}

// TestSessionTokenIsNotStoredInPlaintext is the property that a copy of the
// table cannot be replayed as a cookie.
func TestSessionTokenIsNotStoredInPlaintext(t *testing.T) {
	db := liveDB(t)
	user := mustUser(t, db, "sub-hash")

	token, session, err := db.NewSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if session.TokenHash == token {
		t.Error("the plaintext token was stored")
	}
	if len(session.TokenHash) != 64 {
		t.Errorf("hash is %d chars, want a 64-char hex sha256", len(session.TokenHash))
	}
}

// TestUpsertUserIsIdempotent covers a returning user: the subject is the key, so
// a second sign-in updates rather than colliding.
func TestUpsertUserIsIdempotent(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()
	mustUser(t, db, "sub-twice")

	again, err := db.UpsertUser(ctx, database.User{
		ID: "sub-twice", Email: "renamed@example.com", Name: "New Name",
	})
	if err != nil {
		t.Fatalf("second UpsertUser: %v", err)
	}
	if again.Name != "New Name" {
		t.Errorf("name = %q, want the refreshed one", again.Name)
	}
	if again.Email != "renamed@example.com" {
		t.Errorf("email = %q, want the refreshed one", again.Email)
	}
}

// TestUsageBytesCountsOnlyLiveDeployments is the quota property: a superseded
// deployment must stop counting, or a redeploy would bill for files that are
// already gone.
func TestUsageBytesCountsOnlyLiveDeployments(t *testing.T) {
	db := liveDB(t)
	ctx := context.Background()
	owner := "sub-quota"

	res := mustCreateLive(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), TTL: time.Hour, OwnerID: owner,
	})
	t.Cleanup(func() { cleanupDeployments(t, db, res.Deployment.Prefix) })

	dep, err := db.GetDeployment(ctx, res.Deployment.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	dep.Status = database.StatusLive
	dep.Bytes = 500
	if err := db.DB().WithContext(ctx).Save(dep).Error; err != nil {
		t.Fatalf("save: %v", err)
	}

	if got, err := db.UsageBytes(ctx, owner); err != nil || got != 500 {
		t.Errorf("usage = %d (%v), want 500", got, err)
	}

	dep.Status = database.StatusExpired
	if err := db.DB().WithContext(ctx).Save(dep).Error; err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, err := db.UsageBytes(ctx, owner); err != nil || got != 0 {
		t.Errorf("usage after supersede = %d (%v), want 0", got, err)
	}
}

func countSessions(t *testing.T, db *database.Database) int64 {
	t.Helper()
	var n int64
	if err := db.DB().Model(&database.Session{}).Count(&n).Error; err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}
