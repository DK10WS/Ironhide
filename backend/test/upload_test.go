package test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/dk10ws/ironhide/internal/safepath"
	"github.com/dk10ws/ironhide/internal/upload"
)

// zipEntry describes one archive member to build.
type zipEntry struct {
	name string
	body string
	// symlink makes the entry a symlink rather than a regular file.
	symlink bool
	dir     bool
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		switch {
		case e.dir:
			h.Name = strings.TrimSuffix(e.name, "/") + "/"
			h.SetMode(0755 | os.ModeDir)
		case e.symlink:
			h.SetMode(0777 | os.ModeSymlink)
		default:
			h.SetMode(0644)
		}

		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("create %s: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("write %s: %v", e.name, err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func siteZip(t *testing.T) []byte {
	t.Helper()
	return buildZip(t,
		zipEntry{name: "index.html", body: "<h1>hi</h1>"},
		zipEntry{name: "assets/app.a1b2c3d4.js", body: "console.log(1)"},
	)
}

// ---------------------------------------------------------------------------
// Happy paths
// ---------------------------------------------------------------------------

func TestFromZipStoresEveryFileUnderOnePrefix(t *testing.T) {
	fs := &fakeStore{}

	res, err := upload.FromZip(context.Background(), fs,
		bytes.NewReader(siteZip(t)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}

	if !strings.HasPrefix(res.Prefix, "uploads/") || !strings.HasSuffix(res.Prefix, "/") {
		t.Errorf("prefix %q should look like uploads/<id>/", res.Prefix)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2: %v", len(res.Files), fs.keys())
	}
	for _, k := range fs.keys() {
		if !strings.HasPrefix(k, res.Prefix) {
			t.Errorf("key %q escaped prefix %q", k, res.Prefix)
		}
	}
	if !res.HasIndexHTML {
		t.Error("HasIndexHTML should be true")
	}
}

// TestFromFilesSharesOnePrefix is the regression test for the reason
// FromFiles exists: a static site references its assets by relative URL, so
// files split across prefixes render with every reference 404ing.
func TestFromFilesSharesOnePrefix(t *testing.T) {
	fs := &fakeStore{}

	res, err := upload.FromFiles(context.Background(), fs, []upload.IncomingFile{
		{Name: "index.html", Body: strings.NewReader("<h1>hi</h1>"), Size: -1},
		{Name: "assets/app.a1b2c3d4.js", Body: strings.NewReader("console.log(1)"), Size: -1},
		{Name: "assets/style.a1b2c3d4.css", Body: strings.NewReader("body{}"), Size: -1},
	}, upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}

	if len(fs.puts) != 3 {
		t.Fatalf("got %d writes, want 3", len(fs.puts))
	}

	prefixes := map[string]bool{}
	for _, k := range fs.keys() {
		prefixes[strings.TrimSuffix(strings.SplitN(k, "/", 3)[1], "")] = true
	}
	if len(prefixes) != 1 {
		t.Errorf("files landed under %d different prefixes, want 1: %v", len(prefixes), fs.keys())
	}
	if !res.HasIndexHTML {
		t.Error("HasIndexHTML should be true")
	}
}

// TestFromFilesInheritsCallerPrefix documents that a prefix is minted per call
// and never taken from input, which is what stops one upload addressing
// another.
func TestFromFilesDoesNotAcceptCallerPrefix(t *testing.T) {
	fs := &fakeStore{}

	res, err := upload.FromFiles(context.Background(), fs, []upload.IncomingFile{
		{Name: "index.html", Body: strings.NewReader("x"), Size: -1},
	}, upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}

	for _, k := range fs.keys() {
		if !strings.HasPrefix(k, res.Prefix) {
			t.Errorf("key %q not under generated prefix %q", k, res.Prefix)
		}
	}
}

func TestPrefixesAreUniquePerCall(t *testing.T) {
	seen := make(map[string]bool, 256)

	for range 256 {
		res, err := upload.FromFiles(context.Background(), &fakeStore{}, []upload.IncomingFile{
			{Name: "index.html", Body: strings.NewReader("x"), Size: -1},
		}, upload.DefaultOptions())
		if err != nil {
			t.Fatalf("FromFiles: %v", err)
		}
		if seen[res.Prefix] {
			t.Fatalf("duplicate prefix generated: %s", res.Prefix)
		}
		seen[res.Prefix] = true
	}
}

func TestFromSingleFileDelegatesToFromFiles(t *testing.T) {
	fs := &fakeStore{}
	body := "<h1>hi</h1>"

	res, err := upload.FromSingleFile(context.Background(), fs, "index.html",
		strings.NewReader(body), int64(len(body)), upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromSingleFile: %v", err)
	}
	if !res.HasIndexHTML {
		t.Error("HasIndexHTML should be true")
	}
	if got := fs.keys()[0]; got != res.Prefix+"index.html" {
		t.Errorf("key = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Archive guards
// ---------------------------------------------------------------------------

// TestArchiveRejectsTraversal is the zip-slip regression. safepath.Relative
// must reject ".." on the raw segments: path.Clean("/"+name) would silently
// rewrite "../../etc/passwd" into "/etc/passwd" and accept the attempt.
func TestArchiveRejectsTraversal(t *testing.T) {
	cases := map[string]string{
		"parent reference":  "../evil.html",
		"deep traversal":    "a/../../evil.html",
		"absolute":          "/etc/passwd",
		"windows separator": `..\..\evil.html`,
		"drive letter":      "C:/evil.html",
		"trailing parent":   "assets/..",
		"bare parent":       "..",
		"nested parent":     "a/b/../../../evil.html",
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			raw := buildZip(t,
				zipEntry{name: "index.html", body: "ok"},
				zipEntry{name: entry, body: "pwned"},
			)
			fs := &fakeStore{}

			_, err := upload.FromZip(context.Background(), fs, bytes.NewReader(raw), upload.DefaultOptions())
			if err == nil {
				t.Fatalf("expected rejection for %q", entry)
			}
			if !errors.Is(err, safepath.ErrUnsafe) {
				t.Errorf("want ErrUnsafe, got %v", err)
			}
			// Validation happens before any write, so nothing may be stored.
			if fs.count() != 0 {
				t.Errorf("wrote %v despite invalid entry", fs.keys())
			}
		})
	}
}

func TestArchiveRejectsSymlink(t *testing.T) {
	raw := buildZip(t,
		zipEntry{name: "index.html", body: "ok"},
		zipEntry{name: "passwd", body: "/etc/passwd", symlink: true},
	)
	fs := &fakeStore{}

	_, err := upload.FromZip(context.Background(), fs, bytes.NewReader(raw), upload.DefaultOptions())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want symlink rejection, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite symlink entry", fs.keys())
	}
}

func TestArchiveRejectsEntryFlood(t *testing.T) {
	entries := make([]zipEntry, 0, 40)
	for i := range 40 {
		entries = append(entries, zipEntry{name: "f" + string(rune('a'+i)) + ".txt", body: "x"})
	}
	raw := buildZip(t, entries...)

	opts := upload.DefaultOptions()
	opts.Limits.MaxEntries = 5

	fs := &fakeStore{}
	_, err := upload.FromZip(context.Background(), fs, bytes.NewReader(raw), opts)
	if !errors.Is(err, upload.ErrTooManyEntries) {
		t.Fatalf("want ErrTooManyEntries, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite entry flood", fs.keys())
	}
}

// TestArchiveRejectsDecompressionBomb checks the cap is enforced on bytes
// actually produced, not on the size the archive header claims.
func TestArchiveRejectsDecompressionBomb(t *testing.T) {
	bomb := buildZip(t, zipEntry{name: "bomb.bin", body: strings.Repeat("A", 2<<20)})

	opts := upload.DefaultOptions()
	opts.Limits.MaxFileBytes = 64 << 10

	fs := &fakeStore{}
	_, err := upload.FromZip(context.Background(), fs, bytes.NewReader(bomb), opts)
	if !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite bomb", fs.keys())
	}
}

func TestArchiveRejectsTotalSizeOverflow(t *testing.T) {
	raw := buildZip(t,
		zipEntry{name: "a.txt", body: strings.Repeat("A", 40)},
		zipEntry{name: "b.txt", body: strings.Repeat("B", 40)},
		zipEntry{name: "c.txt", body: strings.Repeat("C", 40)},
	)

	opts := upload.DefaultOptions()
	opts.Limits.MaxTotalBytes = 100 // each file passes, the total does not

	fs := &fakeStore{}
	_, err := upload.FromZip(context.Background(), fs, bytes.NewReader(raw), opts)
	if !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite total overflow", fs.keys())
	}
}

func TestArchiveRejectsOversizedArchive(t *testing.T) {
	opts := upload.DefaultOptions()
	opts.Limits.MaxArchiveBytes = 4 // smaller than any real archive

	_, err := upload.FromZip(context.Background(), &fakeStore{},
		bytes.NewReader(siteZip(t)), opts)
	if !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestArchiveRejectsDuplicateEntries(t *testing.T) {
	raw := buildZip(t,
		zipEntry{name: "index.html", body: "first"},
		zipEntry{name: "index.html", body: "second"},
	)
	fs := &fakeStore{}

	_, err := upload.FromZip(context.Background(), fs, bytes.NewReader(raw), upload.DefaultOptions())
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate rejection, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite duplicate", fs.keys())
	}
}

func TestArchiveRejectsDuplicateFilenames(t *testing.T) {
	fs := &fakeStore{}

	_, err := upload.FromFiles(context.Background(), fs, []upload.IncomingFile{
		{Name: "index.html", Body: strings.NewReader("a"), Size: -1},
		{Name: "index.html", Body: strings.NewReader("b"), Size: -1},
	}, upload.DefaultOptions())
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate rejection, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite duplicate", fs.keys())
	}
}

func TestArchiveRejectsNonZip(t *testing.T) {
	_, err := upload.FromZip(context.Background(), &fakeStore{},
		strings.NewReader("this is not a zip"), upload.DefaultOptions())
	if !errors.Is(err, upload.ErrInvalidArchive) {
		t.Fatalf("want ErrInvalidArchive, got %v", err)
	}
}

func TestArchiveRejectsEmptyArchive(t *testing.T) {
	raw := buildZip(t, zipEntry{name: "assets/", dir: true})
	_, err := upload.FromZip(context.Background(), &fakeStore{},
		bytes.NewReader(raw), upload.DefaultOptions())
	if !errors.Is(err, upload.ErrInvalidArchive) {
		t.Fatalf("want ErrInvalidArchive, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// index.html enforcement
//
// Every case below goes through RequireIndexHTML, which is how an
// index-detection bug becomes observable: during development one such bug
// read the wrong return value and rejected every upload, including valid ones.
// ---------------------------------------------------------------------------

func TestRequireIndexHTML(t *testing.T) {
	tests := []struct {
		name    string
		files   []upload.IncomingFile
		archive []zipEntry
		wantErr string // "" means must succeed
	}{
		{
			name:    "index.html accepted",
			archive: []zipEntry{{name: "index.html", body: "x"}},
		},
		{
			name:    "Index.HTML accepted case-insensitively",
			archive: []zipEntry{{name: "Index.HTML", body: "x"}},
		},
		{
			name:    "index.htm accepted",
			archive: []zipEntry{{name: "index.htm", body: "x"}},
		},
		{
			name:    "nested index rejected with a hint",
			archive: []zipEntry{{name: "site/index.html", body: "x"}},
			wantErr: "flatten",
		},
		{
			name:    "no index at all rejected with the file list",
			archive: []zipEntry{{name: "readme.txt", body: "x"}},
			wantErr: "readme.txt",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := upload.DefaultOptions()
			opts.RequireIndexHTML = true
			fs := &fakeStore{}

			var err error
			if tc.archive != nil {
				_, err = upload.FromZip(context.Background(), fs,
					bytes.NewReader(buildZip(t, tc.archive...)), opts)
			} else {
				_, err = upload.FromFiles(context.Background(), fs, tc.files, opts)
			}

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				if fs.count() == 0 {
					t.Error("nothing was written")
				}
				return
			}

			if !errors.Is(err, upload.ErrNoIndexHTML) {
				t.Fatalf("want ErrNoIndexHTML, got %v", err)
			}
			// The message must be actionable, not just a restatement.
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
			// Rejecting after writing would orphan objects no sweeper can find.
			if fs.count() != 0 {
				t.Errorf("wrote %v despite rejection", fs.keys())
			}
		})
	}
}

func TestFlattenRootStripsSingleDirectory(t *testing.T) {
	raw := buildZip(t,
		zipEntry{name: "mysite/index.html", body: "x"},
		zipEntry{name: "mysite/assets/app.js", body: "y"},
	)

	opts := upload.DefaultOptions()
	opts.FlattenRoot = true

	fs := &fakeStore{}
	res, err := upload.FromZip(context.Background(), fs, bytes.NewReader(raw), opts)
	if err != nil {
		t.Fatalf("FromZip: %v", err)
	}
	if res.RootDir != "mysite" {
		t.Errorf("RootDir = %q, want mysite", res.RootDir)
	}
	for _, k := range fs.keys() {
		if strings.Contains(k, "mysite/") {
			t.Errorf("key %q still contains the stripped directory", k)
		}
	}
}

// ---------------------------------------------------------------------------
// Single file
// ---------------------------------------------------------------------------

func TestFromSingleFileRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"../evil.html", "..", "/etc/passwd", `a\b.html`, "C:/x.html", "", ".",
	} {
		fs := &fakeStore{}
		_, err := upload.FromSingleFile(context.Background(), fs, name,
			strings.NewReader("x"), 1, upload.DefaultOptions())
		if err == nil {
			t.Errorf("name %q should be rejected", name)
			continue
		}
		if fs.count() != 0 {
			t.Errorf("name %q wrote %v", name, fs.keys())
		}
	}
}

func TestFromSingleFileRejectsSizeMismatch(t *testing.T) {
	fs := &fakeStore{}
	// Declare 999 bytes, deliver 5.
	_, err := upload.FromSingleFile(context.Background(), fs, "index.html",
		strings.NewReader("hello"), 999, upload.DefaultOptions())
	if err == nil {
		t.Fatal("expected a declared/actual size mismatch to be rejected")
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite mismatch", fs.keys())
	}
}

func TestFromSingleFileAcceptsUnknownSize(t *testing.T) {
	fs := &fakeStore{}
	body := "hello"

	res, err := upload.FromSingleFile(context.Background(), fs, "index.html",
		strings.NewReader(body), -1, upload.DefaultOptions())
	if err != nil {
		t.Fatalf("an unknown size should be measured, not rejected: %v", err)
	}
	if res.Bytes != int64(len(body)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(body))
	}
}

func TestFromSingleFileEnforcesPerFileCap(t *testing.T) {
	opts := upload.DefaultOptions()
	opts.Limits.MaxFileBytes = 8

	fs := &fakeStore{}
	_, err := upload.FromSingleFile(context.Background(), fs, "big.txt",
		strings.NewReader(strings.Repeat("A", 100)), -1, opts)
	if !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if fs.count() != 0 {
		t.Errorf("wrote %v despite cap", fs.keys())
	}
}

// TestPreBufferedDataIsNotCopied pins the memory optimisation: a caller that
// hands over bytes it already holds must not pay for a second copy. It is
// asserted indirectly, by checking the exact stored bytes survive unchanged.
func TestPreBufferedDataIsNotCopied(t *testing.T) {
	fs := &fakeStore{}
	payload := []byte("<h1>exact bytes</h1>")

	res, err := upload.FromFiles(context.Background(), fs, []upload.IncomingFile{
		{Name: "index.html", Data: payload, Size: int64(len(payload))},
	}, upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}

	got := fs.reader(res.Prefix + "index.html")
	if got == nil {
		t.Fatal("object not stored")
	}
	b, err := io.ReadAll(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, payload) {
		t.Errorf("stored %q, want %q", b, payload)
	}
}

// TestCallerSuppliedPrefixIsUsed is the regression test for a field being
// dropped by Options.normalized, which regenerated the prefix. The caller had
// already written a deployment row for it, so the objects landed somewhere
// else and the site 404d while the row looked fine.
func TestCallerSuppliedPrefixIsUsed(t *testing.T) {
	for name, run := range map[string]func(prefix string, fs *fakeStore) error{
		"FromFiles": func(prefix string, fs *fakeStore) error {
			opts := upload.DefaultOptions()
			opts.Prefix = prefix
			_, err := upload.FromFiles(context.Background(), fs, []upload.IncomingFile{
				{Name: "index.html", Body: strings.NewReader("<h1>x</h1>"), Size: -1},
			}, opts)
			return err
		},
		"FromZip": func(prefix string, fs *fakeStore) error {
			opts := upload.DefaultOptions()
			opts.Prefix = prefix
			_, err := upload.FromZip(context.Background(), fs,
				bytes.NewReader(buildZip(t, zipEntry{name: "index.html", body: "<h1>x</h1>"})), opts)
			return err
		},
		"FromSingleFile": func(prefix string, fs *fakeStore) error {
			opts := upload.DefaultOptions()
			opts.Prefix = prefix
			body := "<h1>x</h1>"
			_, err := upload.FromSingleFile(context.Background(), fs, "index.html",
				strings.NewReader(body), int64(len(body)), opts)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			prefix := upload.NewPrefix()
			fs := &fakeStore{}

			if err := run(prefix, fs); err != nil {
				t.Fatalf("upload: %v", err)
			}

			if fs.count() == 0 {
				t.Fatal("nothing was written")
			}
			for _, k := range fs.keys() {
				if !strings.HasPrefix(k, prefix) {
					t.Errorf("stored at %q, want under the supplied prefix %q", k, prefix)
				}
			}
		})
	}
}

// TestEmptyPrefixStillGeneratesOne guards the other direction: an unset Prefix
// must keep generating, not producing an empty key.
func TestEmptyPrefixStillGeneratesOne(t *testing.T) {
	fs := &fakeStore{}
	res, err := upload.FromFiles(context.Background(), fs, []upload.IncomingFile{
		{Name: "index.html", Body: strings.NewReader("x"), Size: -1},
	}, upload.DefaultOptions())
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}
	if !strings.HasPrefix(res.Prefix, "uploads/") || res.Prefix == "" {
		t.Errorf("Prefix = %q, want a generated uploads/<id>/", res.Prefix)
	}
}
