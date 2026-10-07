package test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

// bytesReader wraps a byte slice without pulling bytes into every test file.
func bytesReader(t *testing.T, b []byte) io.Reader {
	t.Helper()
	return bytes.NewReader(b)
}

// lastSegment returns the trailing path element of a prefix, which is the
// slug newPrefix generated and the label a preview is served under.
//
// The trailing slash matters: newPrefix returns "uploads/<slug>/", so taking
// the last element without trimming first yields an empty string and every
// host derived from it is wrong.
func lastSegment(prefix string) string {
	trimmed := strings.Trim(prefix, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// record persists a deployment row the way the upload handler does, so tests
// exercise the same path production uses.
func record(t *testing.T, db *database.Database, res upload.Result, ttl time.Duration) error {
	t.Helper()

	entries := make([]database.ManifestEntry, 0, len(res.Files))
	for _, f := range res.Files {
		entries = append(entries, database.ManifestEntry{Key: f.Key, Size: f.Size, ETag: f.ETag})
	}

	created, err := db.CreateDeployment(context.Background(), database.CreateDeploymentInput{
		Prefix: res.Prefix,
		Files:  entries,
		TTL:    ttl,
	})
	if err != nil {
		return err
	}

	// Registered here so every caller that records a deployment cleans up,
	// rather than each one remembering to.
	cleanupDeployments(t, db, created.Deployment.Prefix)
	return nil
}

// multipartBody builds a one-part multipart body and its Content-Type.
//
// Written with multipart.Writer rather than by concatenating strings: a
// hand-rolled body is easy to get subtly wrong, and a malformed one produces a
// 400 that looks like an application bug.
func multipartBody(t *testing.T, filename, content string) (body []byte, contentType string) {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// The header is written by hand rather than CreateFormFile so the filename
	// is sent exactly as given, including any directory part. CreateFormFile
	// escapes and would obscure what the client actually transmitted.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	h.Set("Content-Type", "text/plain")

	pw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := pw.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	return buf.Bytes(), mw.FormDataContentType()
}

// doMultipart posts a multipart body to an explicit Host.
func doMultipart(t *testing.T, srv *httptest.Server, host, path string, body []byte, contentType string) *http.Response {
	t.Helper()

	req, err := http.NewRequest("POST", srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	req.Header.Set("Content-Type", contentType)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s (host %s): %v", path, host, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// expireNow ages a deployment row into the past, standing in for time passing.
//
// The row is updated directly rather than recorded with a short TTL because
// CreateDeployment only sets ExpiresAt for a positive TTL.
func expireNow(t *testing.T, db *database.Database, prefix string) {
	t.Helper()

	res := db.DB().Exec(
		"UPDATE deployments SET expires_at = now() - interval '1 minute' WHERE prefix = ?",
		database.NormalizePrefix(prefix),
	)
	if res.Error != nil {
		t.Fatalf("expire deployment: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("expected to age 1 row, aged %d", res.RowsAffected)
	}
}

// uniquePrefix returns a prefix no other test or previous run will use.
//
// Deriving it from t.Name() alone collides: the same test produces the same
// string every run, so rows accumulate and expireNow starts matching more than
// the row it meant to age.
func uniquePrefix(t *testing.T) string {
	t.Helper()

	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	return "uploads/" + hex.EncodeToString(b[:])
}

// cleanupDeployments removes the rows a test created.
//
// Without this the suite leaves live deployments behind that later runs see
// through ListExpired and the sweeper work list.
func cleanupDeployments(t *testing.T, db *database.Database, prefixes ...string) {
	t.Helper()

	for _, p := range prefixes {
		norm := database.NormalizePrefix(p)
		t.Cleanup(func() {
			if err := db.DB().Exec("DELETE FROM deployments WHERE prefix = ?", norm).Error; err != nil {
				t.Logf("cleanup deployments %s: %v", norm, err)
			}
		})
	}
}

// mustCreate inserts a deployment and guarantees its removal.
//
// Every deployment-creating test goes through this rather than calling
// CreateDeployment directly, so no test can forget to register cleanup and
// leave a live row behind for the next run to trip over.
func mustCreate(t *testing.T, db *database.Database, in database.CreateDeploymentInput) *database.CreateDeploymentResult {
	t.Helper()

	res, err := db.CreateDeployment(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	cleanupDeployments(t, db, res.Deployment.Prefix)
	return res
}

// mustSite upserts a site and guarantees its removal.
func mustSite(t *testing.T, db *database.Database, slug, domain, owner string) *database.Site {
	t.Helper()

	site, err := db.UpsertSite(context.Background(), slug, domain, owner)
	if err != nil {
		t.Fatalf("UpsertSite %s: %v", domain, err)
	}
	t.Cleanup(func() {
		if err := db.DB().Exec("DELETE FROM sites WHERE domain = ?", domain).Error; err != nil {
			t.Logf("cleanup site %s: %v", domain, err)
		}
	})
	return site
}

// cleanupPrefixFromBody registers cleanup for a deployment the HTTP handler
// created on our behalf.
//
// An upload that goes through the router inserts its row inside the handler,
// so a test cannot wrap it the way it wraps a direct CreateDeployment call.
// The slug is read back out of preview_url instead.
func cleanupPrefixFromBody(t *testing.T, db *database.Database, body []byte) {
	t.Helper()

	var resp struct {
		PreviewURL string `json:"preview_url"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.PreviewURL == "" {
		return
	}

	slug := slugFromPreviewURL(resp.PreviewURL)
	if slug == "" {
		t.Errorf("could not read a slug out of preview_url %q", resp.PreviewURL)
		return
	}
	cleanupDeployments(t, db, "uploads/"+slug)
}

// slugFromPreviewURL pulls the deployment slug out of a preview URL.
//
// The slug is the first host label, so the scheme and port have to come off
// first: taking the last path segment of "http://<slug>.preview.test/" would
// yield the whole host.
func slugFromPreviewURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}
