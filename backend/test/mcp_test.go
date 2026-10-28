package test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dk10ws/ironhide/internal/core"
	"github.com/dk10ws/ironhide/internal/mcp"
)

// TestMCPUploadThenClaim drives the MCP transport end to end over the SDK's
// in-memory pair.
//
// It asserts the two things an agent depends on: the tools are advertised, and
// a directory on disk becomes a served site with a claim link that can be
// redeemed exactly once.
func TestMCPUploadThenClaim(t *testing.T) {
	db := liveDB(t)
	store := newMemStore()
	cfg := testConfig()

	server := sdk.NewServer(&sdk.Implementation{Name: "ironhide", Version: "test"}, nil)
	mcp.Register(server, core.New(store, db, cfg))

	ctx := context.Background()
	clientSide, serverSide := sdk.NewInMemoryTransports()

	go func() {
		// The server runs until the session closes; the error is the close.
		_ = server.Run(ctx, serverSide)
	}()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	if !names["upload_site"] || !names["claim_site"] {
		t.Fatalf("advertised tools = %v, want upload_site and claim_site", names)
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>from mcp</h1>")
	writeFile(t, filepath.Join(dir, "assets", "app.js"), "console.log(1)")
	// Never part of a site, and enough of it to prove the skip works.
	writeFile(t, filepath.Join(dir, ".git", "config"), "should not be uploaded")

	uploaded := struct {
		URL      string `json:"url"`
		ClaimURL string `json:"claim_url"`
		Bytes    int64  `json:"bytes"`
		Files    int    `json:"files"`
	}{}
	callTool(t, ctx, session, "upload_site", map[string]any{"path": dir}, &uploaded)

	if uploaded.URL == "" {
		t.Fatal("upload_site returned no url")
	}
	if uploaded.ClaimURL == "" {
		t.Fatal("an anonymous drop must come back with a claim link")
	}
	if uploaded.Files != 2 {
		t.Errorf("stored %d files, want 2: .git must be skipped", uploaded.Files)
	}
	if uploaded.Bytes != int64(len("<h1>from mcp</h1>")+len("console.log(1)")) {
		t.Errorf("stored %d bytes, want only the two site files", uploaded.Bytes)
	}

	claimed := struct {
		URL string `json:"url"`
	}{}
	// The URL form is what a caller will have copied, so pass that.
	callTool(t, ctx, session, "claim_site", map[string]any{"token": uploaded.ClaimURL}, &claimed)

	if claimed.URL != uploaded.URL {
		t.Errorf("claimed url = %q, want the same address %q", claimed.URL, uploaded.URL)
	}

	// A token is single use, so the second attempt is a tool error rather than
	// a success with a stale address.
	again, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "claim_site",
		Arguments: map[string]any{"token": uploaded.ClaimURL},
	})
	if err == nil && !again.IsError {
		t.Error("redeeming a spent claim token succeeded")
	}
}

// TestMCPUploadArchive covers the other accepted input: a .zip file whose
// contents sit under a single top-level directory, the shape a downloaded
// repository has.
func TestMCPUploadArchive(t *testing.T) {
	db := liveDB(t)
	store := newMemStore()
	cfg := testConfig()

	server := sdk.NewServer(&sdk.Implementation{Name: "ironhide", Version: "test"}, nil)
	mcp.Register(server, core.New(store, db, cfg))

	ctx := context.Background()
	clientSide, serverSide := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverSide) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, body := range map[string]string{
		"site-main/index.html":    "<h1>wrapped</h1>",
		"site-main/assets/app.js": "console.log(2)",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	path := filepath.Join(t.TempDir(), "site.zip")
	if err := os.WriteFile(path, archive.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	uploaded := struct {
		URL   string `json:"url"`
		Files int    `json:"files"`
	}{}
	callTool(t, ctx, session, "upload_site", map[string]any{"path": path}, &uploaded)

	if uploaded.URL == "" {
		t.Fatal("upload_site returned no url")
	}
	if uploaded.Files != 2 {
		t.Errorf("stored %d files, want 2", uploaded.Files)
	}
}

// callTool invokes a tool and decodes its structured result into out.
func callTool(t *testing.T, ctx context.Context, session *sdk.ClientSession, name string, args map[string]any, out any) {
	t.Helper()

	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s failed: %s", name, contentText(res))
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshal result: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: decode result: %v", name, err)
	}
}

func contentText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
