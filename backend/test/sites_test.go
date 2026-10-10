package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
)

// signedInServer returns a server and a live session cookie for an account.
func signedInServer(t *testing.T, cfg config.Config, owner string) (*httptest.Server, *database.Database, string) {
	t.Helper()
	srv, db := newAuthServer(t, cfg)

	user := mustUser(t, db, owner)
	token, _, err := db.NewSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return srv, db, token
}

func smallQuotaConfig(limit int64) config.Config {
	c := authConfig()
	c.Quota.BytesPerUser = limit
	return c
}

// uploadAs posts a site and returns the response body and status.
func uploadAs(t *testing.T, srv *httptest.Server, token, path, filename, content string) (int, string) {
	t.Helper()
	body, ct := multipartBody(t, filename, content)

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = controlHost
	req.Header.Set("Content-Type", ct)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "ironhide_session", Value: token})
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestSignedInUploadBecomesAnOwnedSite is the central property: a session turns
// a temporary drop into a permanent site with no claim link.
func TestSignedInUploadBecomesAnOwnedSite(t *testing.T) {
	srv, db, token := signedInServer(t, authConfig(), "sub-owner")

	code, body := uploadAs(t, srv, token, "/api/upload/files", "index.html", "<h1>owned</h1>")
	if code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201: %s", code, body)
	}
	if strings.Contains(body, "claim_url") {
		t.Errorf("an owned upload returned a claim link: %s", body)
	}
	if !strings.Contains(body, `"owned":true`) {
		t.Errorf("body does not say the upload is owned: %s", body)
	}
	// Permanent means no deadline at all, not a far-future one.
	if !strings.Contains(body, `"expires_at":null`) {
		t.Errorf("an owned upload was given an expiry: %s", body)
	}

	var sites []OwnedSiteForTest
	listOwned(t, db, "sub-owner", &sites)
	if len(sites) != 1 {
		t.Fatalf("owner has %d sites, want 1", len(sites))
	}
	if !sites[0].Live {
		t.Error("the site is not live")
	}
}

// TestAnonymousUploadStillWorks is the other half: removing accounts must not
// remove the drop-and-claim path.
func TestAnonymousUploadStillWorks(t *testing.T) {
	srv, db, _ := signedInServer(t, authConfig(), "sub-unused")

	code, body := uploadAs(t, srv, "", "/api/upload/files", "index.html", "<h1>anon</h1>")
	if code != http.StatusCreated {
		t.Fatalf("anonymous upload = %d, want 201: %s", code, body)
	}
	if !strings.Contains(body, "claim_url") {
		t.Errorf("an anonymous upload lost its claim link: %s", body)
	}

	var sites []OwnedSiteForTest
	listOwned(t, db, "sub-unused", &sites)
	if len(sites) != 0 {
		t.Errorf("an anonymous upload created %d owned sites", len(sites))
	}
}

// TestMySitesRequiresSession is the authorization boundary.
func TestMySitesRequiresSession(t *testing.T) {
	srv, _, _ := signedInServer(t, authConfig(), "sub-auth")

	for _, path := range []string{"/api/my/sites", "/api/my/usage"} {
		if got := do(t, srv, "GET", controlHost, path, nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("GET %s with no session = %d, want 401", path, got)
		}
	}
}

// TestSitesAreScopedToTheirOwner is the property that one account cannot see or
// touch another's sites.
func TestSitesAreScopedToTheirOwner(t *testing.T) {
	srv, _, mine := signedInServer(t, authConfig(), "sub-mine")

	_, _, theirs := signedInServer(t, authConfig(), "sub-theirs-delete")
	code, _ := uploadAs(t, srv, theirs, "/api/upload/files", "index.html", "<h1>theirs</h1>")
	if code != http.StatusCreated {
		t.Fatalf("theirs upload = %d, want 201", code)
	}

	// Mine sees nothing of theirs.
	req := newGet(t, srv, "/api/my/sites", mine)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"sites":[]`) {
		t.Errorf("owner sees another account's sites: %s", body)
	}
}

// TestDeleteRemovesOnlyYourOwnSite checks the cross-account delete is refused
// as not found rather than forbidden, so it cannot confirm the id exists.
func TestDeleteRemovesOnlyYourOwnSite(t *testing.T) {
	srv, db, theirs := signedInServer(t, authConfig(), "sub-theirs-delete")

	code, _ := uploadAs(t, srv, theirs, "/api/upload/files", "index.html", "<h1>theirs</h1>")
	if code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}

	var sites []OwnedSiteForTest
	listOwned(t, db, "sub-theirs-delete", &sites)
	if len(sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(sites))
	}
	id := sites[0].ID

	_, _, mine := signedInServer(t, authConfig(), "sub-mine")
	req := newDelete(t, srv, "/api/my/sites/"+itoa(id), mine)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleting another account's site = %d, want 404", resp.StatusCode)
	}

	// And it must still be there.
	var after []OwnedSiteForTest
	listOwned(t, db, "sub-theirs-delete", &after)
	if len(after) != 1 {
		t.Error("the other account's site was deleted anyway")
	}
}

// TestQuotaRefusesAnOversizedUpload is the property the whole allowance rests
// on, checked before any bytes are written.
func TestQuotaRefusesAnOversizedUpload(t *testing.T) {
	// 40 bytes of allowance, and the upload is larger.
	srv, _, token := signedInServer(t, smallQuotaConfig(40), "sub-quota-small")

	code, body := uploadAs(t, srv, token, "/api/upload/files", "index.html", strings.Repeat("x", 500))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, want 413: %s", code, body)
	}
	if !strings.Contains(body, "quota") {
		t.Errorf("the refusal does not mention the quota: %s", body)
	}
}

// TestQuotaIsMeasuredAcrossSites proves the allowance is across an account, not
// per site: two small sites that each fit can still exhaust it together.
func TestQuotaIsMeasuredAcrossSites(t *testing.T) {
	srv, _, token := signedInServer(t, smallQuotaConfig(120), "sub-quota-two")

	if code, body := uploadAs(t, srv, token, "/api/upload/files", "index.html", strings.Repeat("a", 50)); code != http.StatusCreated {
		t.Fatalf("first upload = %d, want 201: %s", code, body)
	}
	code, body := uploadAs(t, srv, token, "/api/upload/files", "index.html", strings.Repeat("b", 100))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second upload = %d, want 413: %s", code, body)
	}
}

// TestQuotaCreditsReplacedBytes is the property that redeploying does not get
// more expensive every time: the bytes it replaces stop counting.
func TestQuotaCreditsReplacedBytes(t *testing.T) {
	srv, db, token := signedInServer(t, smallQuotaConfig(120), "sub-quota-redeploy")

	if code, body := uploadAs(t, srv, token, "/api/upload/files", "index.html", strings.Repeat("a", 50)); code != http.StatusCreated {
		t.Fatalf("first upload = %d: %s", code, body)
	}

	var sites []OwnedSiteForTest
	listOwned(t, db, "sub-quota-redeploy", &sites)
	if len(sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(sites))
	}
	id := itoa(sites[0].ID)

	// A same-sized redeploy is refused without the credit, and allowed with it.
	body, ct := multipartBody(t, "index.html", strings.Repeat("c", 100))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/my/sites/"+id+"/deploy", strings.NewReader(string(body)))
	req.Host = controlHost
	req.Header.Set("Content-Type", ct)
	req.AddCookie(&http.Cookie{Name: "ironhide_session", Value: token})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeploy = %d, want 201: %s", resp.StatusCode, out)
	}

	// The address must not move.
	var after []OwnedSiteForTest
	listOwned(t, db, "sub-quota-redeploy", &after)
	if len(after) != 1 {
		t.Fatalf("expected 1 site after redeploy, got %d", len(after))
	}
	if after[0].Slug != sites[0].Slug {
		t.Errorf("redeploy changed the slug %q -> %q", sites[0].Slug, after[0].Slug)
	}
}

// OwnedSiteForTest mirrors the fields the dashboard reads, so a test can assert
// on them without reaching into the query.
type OwnedSiteForTest struct {
	ID   uint
	Slug string
	Live bool
}

func listOwned(t *testing.T, db *database.Database, owner string, out *[]OwnedSiteForTest) {
	t.Helper()
	sites, err := db.ListOwnedSites(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListOwnedSites %s: %v", owner, err)
	}
	for _, s := range sites {
		*out = append(*out, OwnedSiteForTest{ID: s.ID, Slug: s.Slug, Live: s.Live})
	}
}

func newGet(t *testing.T, srv *httptest.Server, path, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = controlHost
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "ironhide_session", Value: token})
	}
	return req
}

func newDelete(t *testing.T, srv *httptest.Server, path, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = controlHost
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "ironhide_session", Value: token})
	}
	return req
}

func itoa(v uint) string { return strconv.FormatUint(uint64(v), 10) }
func itoaUnused(v uint) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
