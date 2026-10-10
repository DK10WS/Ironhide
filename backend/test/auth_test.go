package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// An empty name means the request carries no cookie, which is how the start
	// of the flow is issued before there is anything to send.
	if name != "" {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}

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

// TestSuccessfulSignInIssuesAUsableSession is the other half of the flow, and
// the half a live Google account is otherwise the only way to reach.
//
// Everything past the consent screen — exchanging the code with the client
// secret, reading the profile, upserting the account, and issuing the cookie
// that /api/me then accepts — happens without a browser in front of it, so it
// can be driven against a stand-in for Google's endpoints. Google itself is
// still what has to say yes on a real deployment; this covers the code on this
// side of that line.
func TestSuccessfulSignInIssuesAUsableSession(t *testing.T) {
	var gotTokenForm url.Values
	var gotAuthHeader string

	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			gotTokenForm = r.Form
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"at-123","token_type":"Bearer","expires_in":3600}`))
		case "/userinfo":
			gotAuthHeader = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sub":"google-123","email":"ada@example.com",` +
				`"email_verified":true,"name":"Ada Lovelace","picture":"https://example.com/a.png"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(google.Close)

	// The stand-in always reports the same account, so cleanup keys on that.
	const email = "ada@example.com"

	cfg := authConfig()
	cfg.Google.TokenURL = google.URL + "/token"
	cfg.Google.UserInfoURL = google.URL + "/userinfo"
	cfg.Google.AuthURL = google.URL + "/authorize"

	srv, db := newAuthServer(t, cfg)
	t.Cleanup(func() {
		if err := db.DB().Exec("DELETE FROM sessions WHERE user_id IN "+
			"(SELECT id FROM users WHERE email = ?)", email).Error; err != nil {
			t.Logf("cleanup sessions: %v", err)
		}
		if err := db.DB().Exec("DELETE FROM users WHERE email = ?", email).Error; err != nil {
			t.Logf("cleanup user: %v", err)
		}
	})

	// Start the flow and take the state from the cookie Google would get back.
	start := withCookie(t, srv, "GET", "/api/auth/google", "", "")
	if start.StatusCode != http.StatusFound {
		t.Fatalf("start = %d, want a redirect", start.StatusCode)
	}
	var state string
	for _, c := range start.Cookies() {
		if c.Name == "ironhide_oauth_state" && c.HttpOnly {
			state = c.Value
		}
	}
	if state == "" {
		t.Fatal("no HttpOnly state cookie was issued")
	}

	resp := withCookie(t, srv, "GET",
		"/api/auth/google/callback?code=good-code&state="+url.QueryEscape(state),
		"ironhide_oauth_state", state)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback = %d, want a redirect home", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "error=") {
		t.Fatalf("callback redirected with an error: %s", loc)
	}

	// The exchange must carry the secret, not just the code.
	if gotTokenForm.Get("code") != "good-code" {
		t.Errorf("token request sent code %q", gotTokenForm.Get("code"))
	}
	if gotTokenForm.Get("client_secret") != cfg.Google.ClientSecret {
		t.Error("token request did not carry the client secret")
	}
	if gotTokenForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", gotTokenForm.Get("grant_type"))
	}
	if gotAuthHeader != "Bearer at-123" {
		t.Errorf("userinfo Authorization = %q", gotAuthHeader)
	}

	// The session cookie is the whole point, so check its attributes too.
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ironhide_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie was issued")
	}
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if session.Path != "/api" {
		t.Errorf("session cookie path = %q, want /api", session.Path)
	}

	// And it must actually authenticate.
	me := withCookie(t, srv, "GET", "/api/me", "ironhide_session", session.Value)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/api/me = %d, want the session to be accepted", me.StatusCode)
	}
	body, _ := io.ReadAll(me.Body)
	// The profile comes from the stand-in, so these are its values.
	if !strings.Contains(string(body), email) {
		t.Errorf("/api/me = %s, want the email from the profile", body)
	}
	if !strings.Contains(string(body), "google-123") {
		t.Errorf("/api/me = %s, want the subject as the account id", body)
	}
}

// TestCallbackRejectsMismatchedState is the CSRF property. A callback whose
// state does not match the cookie must not create a session, or an attacker
// could sign a victim into an account they control.
func TestCallbackRejectsMismatchedState(t *testing.T) {
	srv, db := newAuthServer(t, authConfig())
	// Counted as a delta, not as zero. These tests share the development
	// database with everything else, and asserting an absolute zero fails as
	// soon as anyone has signed in for real, which says nothing about this
	// code. The property is that no session appears, not that none ever did.
	before := countSessions(t, db)

	resp := withCookie(t, srv, "GET",
		"/api/auth/google/callback?code=abc&state=wrong", "ironhide_oauth_state", "right")

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback with a bad state = %d, want a redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("redirect %q should carry an error, not sign anyone in", loc)
	}
	if got := countSessions(t, db) - before; got != 0 {
		t.Errorf("a rejected callback created %d sessions", got)
	}
}

// TestCallbackWithoutStateCookieIsRejected covers the same property with no
// cookie at all.
func TestCallbackWithoutStateCookieIsRejected(t *testing.T) {
	srv, db := newAuthServer(t, authConfig())
	before := countSessions(t, db)

	resp := do(t, srv, "GET", controlHost, "/api/auth/google/callback?code=abc&state=whatever", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback without a state cookie = %d, want a redirect", resp.StatusCode)
	}
	if got := countSessions(t, db) - before; got != 0 {
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
