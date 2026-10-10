package test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk10ws/ironhide/internal/api"
	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

const (
	controlHost = "ironhide.test"
	dataSuffix  = ".preview.test"
)

func testConfig() config.Config {
	return config.Config{
		ControlHost:    controlHost,
		DataHostSuffix: dataSuffix,
		DataScheme:     "https",
		PublicURL:      "https://" + controlHost,
		Upload: config.Upload{
			MaxBytes:     8 << 20,
			ArchiveBytes: 4 << 20,
			FileBytes:    1 << 20,
			TotalBytes:   4 << 20,
			MaxEntries:   64,
			TTL:          time.Hour,
		},
	}
}

// memStore satisfies the api package's storage interface: writes for the
// upload routes, reads for the data plane.
// memStore records what it was given so the serve path can read it back.
//
// The mutex guards objects because upload.write fans its writes out with
// errgroup, and ObjectStore is documented as safe for concurrent use. The
// embedded fakeStore has its own lock for puts; this one covers the map that
// GetObject reads.
type memStore struct {
	fakeStore
	mu      sync.Mutex
	objects map[string]string
}

func newMemStore() *memStore {
	return &memStore{objects: map[string]string{}}
}

func (m *memStore) PutObject(ctx context.Context, key string, body io.Reader, size int64) (string, error) {
	etag, err := m.fakeStore.PutObject(ctx, key, body, size)
	if err != nil {
		return "", err
	}
	b := m.fakeStore.reader(key)
	raw, _ := io.ReadAll(b)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = string(raw)
	return etag, nil
}

func (m *memStore) GetObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := m.objects[key]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(v)), int64(len(v)), nil
}

// newTestServer builds the router over a real database and an in-memory store.
//
// It skips unless TEST_POSTGRES_HOST is set: Handler.db is a concrete
// *database.Database rather than an interface, so exercising the HTTP layer
// means a real Postgres.
func newTestServer(t *testing.T) (*httptest.Server, *database.Database, *memStore) {
	t.Helper()
	return newTestServerWithConfig(t, testConfig())
}

// newTestServerWithConfig is the seam for the tests that need a configuration
// testConfig does not produce, such as a data suffix that pins a port.
func newTestServerWithConfig(t *testing.T, cfg config.Config) (*httptest.Server, *database.Database, *memStore) {
	t.Helper()

	host := os.Getenv("TEST_POSTGRES_HOST")
	if host == "" {
		t.Skip("set TEST_POSTGRES_HOST to run HTTP tests")
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
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := newMemStore()
	srv := httptest.NewServer(api.NewRouter(cfg, store, db))
	t.Cleanup(srv.Close)

	return srv, db, store
}

// do issues a request with an explicit Host, which is how the two planes are
// told apart.
func do(t *testing.T, srv *httptest.Server, method, host, path string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (host %s): %v", method, path, host, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// ---------------------------------------------------------------------------
// Plane isolation
// ---------------------------------------------------------------------------

// TestControlRoutesOnlyAnswerOnTheControlHost is the property that keeps
// uploaded content away from anything authenticated: a request arriving on a
// preview host must never reach an upload handler.
func TestControlRoutesOnlyAnswerOnTheControlHost(t *testing.T) {
	srv, db, _ := newTestServer(t)

	upload := func(host string) int {
		body, ct := multipartBody(t, "index.html", "<h1>x</h1>")
		resp := doMultipart(t, srv, host, "/api/upload/files", body, ct)
		if resp.StatusCode == http.StatusCreated {
			got, _ := io.ReadAll(resp.Body)
			cleanupPrefixFromBody(t, db, got)
		}
		return resp.StatusCode
	}

	if got := upload(controlHost); got != http.StatusCreated {
		t.Errorf("POST /api/upload/files on the control host = %d, want 201", got)
	}
	if got := upload("4a50b157cc8b9cd3" + dataSuffix); got != http.StatusNotFound {
		t.Errorf("POST /api/upload/files on a data host = %d, want 404", got)
	}
}

func TestHealthOnlyAnswersOnTheControlHost(t *testing.T) {
	srv, _, _ := newTestServer(t)

	if got := do(t, srv, "GET", controlHost, "/api/health", nil).StatusCode; got != http.StatusOK {
		t.Errorf("GET /api/health on the control host = %d, want 200", got)
	}
	if got := do(t, srv, "GET", "4a50b157cc8b9cd3"+dataSuffix, "/api/health", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET /api/health on a data host = %d, want 404", got)
	}
}

// TestDataHostSuffixIsAnchored is the regression for a real bug: the slug was
// extracted with strings.Index, so "abc.preview.test.attacker.example" also
// resolved to the deployment named abc.
func TestDataHostSuffixIsAnchored(t *testing.T) {
	srv, db, store := newTestServer(t)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}

	slug := lastSegment(res.Prefix)

	cases := map[string]int{
		slug + dataSuffix:                       http.StatusOK,
		slug + dataSuffix + ".attacker.example": http.StatusNotFound,
		"evil." + slug + dataSuffix:             http.StatusNotFound,
		"nothex" + dataSuffix:                   http.StatusNotFound,
		// DNS is case-insensitive, so an uppercased label is the same host and
		// must resolve. Matching is deliberately case-folded rather than
		// exact, so a client that uppercases the hostname still gets the site.
		strings.ToUpper(slug) + dataSuffix: http.StatusOK,
	}

	for host, want := range cases {
		if got := do(t, srv, "GET", host, "/", nil).StatusCode; got != want {
			t.Errorf("GET / with host %q = %d, want %d", host, got, want)
		}
	}
}

// TestFrameAncestorsDefaultsToNone is the property that keeps an upload from
// being laid over a convincing fake of another page. It is the default because
// nothing needs framing until the app asks to be named.
func TestFrameAncestorsDefaultsToNone(t *testing.T) {
	srv, db, store := newTestServer(t)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}

	host := lastSegment(res.Prefix) + dataSuffix
	got := do(t, srv, "GET", host, "/", nil).Header.Get("Content-Security-Policy")
	if !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q, want frame-ancestors 'none' when FrontendOrigin is unset", got)
	}
}

// TestFrameAncestorsAllowsOnlyTheNamedOrigin checks that naming one origin
// opens framing to that origin and no other.
func TestFrameAncestorsAllowsOnlyTheNamedOrigin(t *testing.T) {
	cfg := testConfig()
	cfg.FrontendOrigin = "http://localhost:7210"

	srv, db, store := newTestServerWithConfig(t, cfg)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	host := lastSegment(res.Prefix) + dataSuffix

	got := do(t, srv, "GET", host, "/", nil).Header.Get("Content-Security-Policy")
	if !strings.Contains(got, "frame-ancestors http://localhost:7210") {
		t.Errorf("CSP = %q, want the named origin in frame-ancestors", got)
	}
	// '*' would let any site frame an upload, which is the thing being traded away.
	if strings.Contains(got, "*") {
		t.Errorf("CSP = %q, must not allow any ancestor", got)
	}
	// The rest of the policy is not up for negotiation with the preview feature.
	if !strings.Contains(got, "base-uri 'none'") || !strings.Contains(got, "form-action 'none'") {
		t.Errorf("CSP = %q, lost base-uri or form-action", got)
	}
}

// TestDataHostSuffixMayPinAPort is the regression for a real 404. The Host
// header has its port cut off before the suffix is compared, so a suffix that
// pinned one could never match and every preview resolved to nothing.
//
// The port has to survive in the built URL, because that is how the URL reaches
// the listener, and be dropped before the comparison, because the Host header
// no longer carries it. Nothing else can satisfy both.
func TestDataHostSuffixMayPinAPort(t *testing.T) {
	suffix := dataSuffix + ":8443"

	cfg := testConfig()
	cfg.DataHostSuffix = suffix

	srv, db, store := newTestServerWithConfig(t, cfg)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	slug := lastSegment(res.Prefix)

	// The port is not part of the deployment's identity, only of how the URL
	// reaches it, so a Host without one names the same origin behind a proxy
	// that terminated it.
	resolves := []string{
		slug + suffix,
		slug + dataSuffix,
		strings.ToUpper(slug) + suffix,
	}
	for _, host := range resolves {
		if got := do(t, srv, "GET", host, "/", nil).StatusCode; got != http.StatusOK {
			t.Errorf("GET / with host %q = %d, want 200", host, got)
		}
	}

	// The anchoring has to survive the strip, or this fix would trade a 404 for a
	// way to address a deployment from a domain that has nothing to do with it.
	refused := []string{
		slug + suffix + ".attacker.example",
		"evil." + slug + suffix,
		"nothex" + suffix,
		slug + dataSuffix + ".attacker.example",
	}
	for _, host := range refused {
		if got := do(t, srv, "GET", host, "/", nil).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET / with host %q = %d, want 404", host, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Serving
// ---------------------------------------------------------------------------

func TestServesIndexAndAssetsWithHeaders(t *testing.T) {
	srv, db, store := newTestServer(t)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	slug := lastSegment(res.Prefix)
	host := slug + dataSuffix

	// The index must not be cached, or a redeploy is invisible to visitors.
	resp := do(t, srv, "GET", host, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", got)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("index Content-Type = %q", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if resp.Header.Get("X-Robots-Tag") == "" {
		t.Error("missing noindex")
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", got)
	}
	// An asset allowlist that admits only 'self' rejects inline scripts, inline
	// styles, data: URIs and CDN files, so any such site arrives as a blank
	// page. Only the directives that cost a legitimate site nothing belong here.
	for _, banned := range []string{"default-src", "script-src", "style-src"} {
		if strings.Contains(resp.Header.Get("Content-Security-Policy"), banned) {
			t.Errorf("CSP = %q, should not carry %q", resp.Header.Get("Content-Security-Policy"), banned)
		}
	}

	// A content-addressed asset is immutable.
	asset := do(t, srv, "GET", host, "/assets/app.a1b2c3d4.js", nil)
	if asset.StatusCode != http.StatusOK {
		t.Fatalf("GET asset = %d", asset.StatusCode)
	}
	if got := asset.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", got)
	}
	if got := asset.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("asset Content-Type = %q", got)
	}
}

// TestExpiredDropIsNotServed proves the claim window holds even though no
// sweeper has run yet.
func TestExpiredDropIsNotServed(t *testing.T) {
	srv, db, store := newTestServer(t)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	// Record with a live TTL, then age the row. Passing a negative TTL would
	// not work: CreateDeployment only sets ExpiresAt for a positive TTL, so a
	// negative one yields a permanent deployment rather than an expired one.
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	expireNow(t, db, res.Prefix)

	host := lastSegment(res.Prefix) + dataSuffix
	if got := do(t, srv, "GET", host, "/", nil).StatusCode; got != http.StatusGone {
		t.Errorf("GET / on an expired drop = %d, want 410", got)
	}
}

func TestUnknownSlugIsNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)

	if got := do(t, srv, "GET", "0123456789abcdef"+dataSuffix, "/", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET / with an unknown slug = %d, want 404", got)
	}
}

func TestMissingAssetIsNotFound(t *testing.T) {
	srv, db, store := newTestServer(t)

	res, err := upload.FromZip(context.Background(), store,
		bytesReader(t, siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if err := record(t, db, res, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}

	host := lastSegment(res.Prefix) + dataSuffix
	if got := do(t, srv, "GET", host, "/nope.css", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET a missing asset = %d, want 404", got)
	}
}

// TestUploadRejectionsAreActionable checks the status codes a client can act
// on, since these are the difference between a usable error and a support
// ticket.
func TestUploadRejectionsAreActionable(t *testing.T) {
	srv, db, _ := newTestServer(t)

	send := func(filename string, content []byte) (int, string) {
		body, ct := multipartBody(t, filename, string(content))
		resp := doMultipart(t, srv, controlHost, "/api/upload/files", body, ct)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusCreated {
			cleanupPrefixFromBody(t, db, b)
		}
		return resp.StatusCode, string(b)
	}

	if code, _ := send("../../../etc/passwd", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("traversal filename = %d, want 400", code)
	}
	if code, _ := send("readme.txt", []byte("x")); code != http.StatusUnprocessableEntity {
		t.Errorf("no index.html = %d, want 422", code)
	}
	if code, _ := send("index.html", []byte("<h1>ok</h1>")); code != http.StatusCreated {
		t.Errorf("valid upload = %d, want 201", code)
	}
}

func TestNonMultipartBodyIsRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp := do(t, srv, "POST", controlHost, "/api/upload/files", strings.NewReader("raw"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-multipart upload = %d, want 400", resp.StatusCode)
	}
}

// TestClaimTokenIsNotLogged is a regression test for a real leak: the request
// logger recorded the URL path verbatim, and for POST /api/claim/<token> that path
// is the only credential on the route.
func TestClaimTokenIsNotLogged(t *testing.T) {
	// This asserts the observable property rather than reaching into the
	// logger: a real request is issued and the emitted log lines are scanned
	// for the token.
	srv, db, _ := newTestServer(t)

	res := mustCreate(t, db, database.CreateDeploymentInput{
		Prefix: uniquePrefix(t), Files: manifest(), TTL: time.Hour,
	})
	_ = res

	token, err := database.NewClaimToken()
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	do(t, srv, "POST", controlHost, "/api/claim/"+token, nil)

	if strings.Contains(buf.String(), token) {
		t.Errorf("the claim token leaked into the logs:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "/api/claim/***") {
		t.Errorf("expected a redacted path, got:\n%s", buf.String())
	}
}
