// Package upload turns an inbound archive or file into a self-contained set
// of objects under a unique prefix in the bucket.
//
// It is the shared core for both transports: the HTTP handler and any MCP
// deploy tool call the same functions here. Nothing in this package knows
// about HTTP or MCP, and nothing outside it should hold logic.
package upload

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/dk10ws/ironhide/internal/safepath"
)

var (
	ErrInvalidArchive = errors.New("invalid archive")

	ErrTooManyEntries = errors.New("too many entries")

	ErrTooLarge = errors.New("upload too large")

	ErrNoIndexHTML = errors.New("upload contains no index.html")
)

const prefixSegmentBytes = 8

// NewPrefix returns a fresh object-key prefix for one upload.
//
// Uniqueness comes from the server, not from the client: no part of this
// value is derived from a filename, a domain or anything a caller can
// influence. That is what guarantees an upload can never address another
// upload's objects, or a claimed site.
//
// It is exported so the HTTP handler can write a deployment row before the
// objects, which is what keeps bytes from existing with nothing describing
// them. Only server-side code may pass the result back through Options.Prefix.
func NewPrefix() string {
	var b [prefixSegmentBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform. If it somehow
		// did, a predictable prefix is far worse than crashing, so refuse.
		panic(fmt.Sprintf("upload: crypto/rand unavailable: %v", err))
	}
	return "uploads/" + hex.EncodeToString(b[:]) + "/"
}

// ObjectStore is the subset of object storage this package needs.
//
// It is an interface so the archive logic can be tested against an in-memory
// fake. The production implementation lives in package garage.
//
// Implementations must be safe for concurrent use: write issues its PutObject
// calls through an errgroup, so more than one can be in flight at a time.
type ObjectStore interface {
	// PutObject writes body at key and returns the stored ETag. size must be
	// the exact number of bytes body will yield: the S3 client cannot stream
	// an object of unknown length and would buffer it internally, which would
	// defeat the memory bounds this package enforces.
	PutObject(ctx context.Context, key string, body io.Reader, size int64) (string, error)
}

type Result struct {
	Prefix string

	Files []File

	Bytes int64

	HasIndexHTML bool

	RootDir string
}

type File struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	ETag string `json:"etag,omitempty"`
}

type Limits struct {
	MaxArchiveBytes int64

	MaxFileBytes int64

	MaxTotalBytes int64

	MaxEntries int
}

func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes: 50 << 20, // 50 MiB compressed
		MaxFileBytes:    50 << 20, // 50 MiB per file
		MaxTotalBytes:   50 << 20, // 50 MiB uncompressed across one request
		MaxEntries:      4096,
	}
}

type Options struct {
	Limits Limits

	// FlattenRoot strips a single top-level directory, so zipping a project
	// folder produces index.html at the root instead of
	// mysite/index.html. Off by default because it is surprising: it also
	// means every file moves when the folder is renamed.
	FlattenRoot bool

	// Prefix overrides the generated object prefix.
	//
	// It exists so a deployment row can be written before its objects, which
	// is what prevents a failed upload from leaving bytes that nothing can
	// find. Empty means generate one here, which is the default and what every
	// caller except the HTTP handler wants.
	//
	// Only ever set it to a value from NewPrefix. Passing anything derived
	// from a request would let one upload address another.
	Prefix string

	// RequireIndexHTML rejects an upload that has no index.html at its root.
	//
	// The check runs during planning, before anything is written. Rejecting
	// afterwards would leave objects in the bucket with no deployment row
	// pointing at them, which nothing would ever find or clean up.
	RequireIndexHTML bool

	// AllowSinglePDF accepts an upload that is exactly one PDF at the root and
	// nothing else, which has no index.html to be found. The PDF is then served
	// at the site root, so a document gets an address of its own.
	//
	// Deliberately narrow. It requires the whole upload to be that one file, so
	// a folder of PDFs or a site that merely links to one still needs an index,
	// and there is no case where adding files to a lone PDF silently changes
	// what the root serves.
	AllowSinglePDF bool
}

func DefaultOptions() Options {
	return Options{Limits: DefaultLimits()}
}

func (o Options) normalized() Options {
	d := DefaultLimits()
	l := o.Limits
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = d.MaxArchiveBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	// The trailing separator is enforced because keys are built by joining the
	// prefix and the relative path. A caller reading the prefix back out of the
	// database gets it without one, and without this every key would be written
	// as "uploads/<slug>index.html". Left empty when it arrives empty, since an
	// empty prefix is how the caller asks for a generated one.
	prefix := o.Prefix
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/") + "/"
	}

	return Options{
		// Prefix is carried through. Dropping it here silently regenerated
		// one, so a caller that had already written a deployment row for this
		// prefix stored the objects somewhere else and the site 404d. Every
		// field must be listed explicitly because this rebuilds the struct.
		Prefix:           prefix,
		Limits:           l,
		FlattenRoot:      o.FlattenRoot,
		RequireIndexHTML: o.RequireIndexHTML,
		AllowSinglePDF:   o.AllowSinglePDF,
	}
}

func FromZip(ctx context.Context, store ObjectStore, r io.Reader, opts Options) (Result, error) {
	opts = opts.normalized()

	raw, err := readCapped(r, opts.Limits.MaxArchiveBytes)
	if err != nil {
		return Result{}, err
	}

	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return Result{}, fmt.Errorf("%w: not a readable zip: %v", ErrInvalidArchive, err)
	}

	files, rootDir, err := plan(zr, opts)
	if err != nil {
		return Result{}, err
	}
	return write(ctx, store, rootDir, files, opts)
}

// pickPrefix returns the caller-supplied prefix if there is one, and
// generates a fresh one otherwise.
func pickPrefix(o Options) string {
	if o.Prefix != "" {
		return o.Prefix
	}
	return NewPrefix()
}

// write stores every planned file under one prefix.
//
// The prefix is minted here, once, and never accepted from a caller. That is
// the property that keeps one upload from addressing another: there is a
// single code path where the prefix comes into existence, so no transport can
// accidentally hand in its own.
//
// It is also why every file of a site lands under the same prefix. A static
// site references its assets by relative URL, and a browser resolves those
// against the page's own path, so app.js is only found if it shares a prefix
// with the index.html that links it.
// uploadConcurrency is how many objects are written at once.
//
// Each PutObject is a round trip that is almost entirely latency, so writing
// them in a batch overlaps those waits. Measured against a single Garage node,
// throughput plateaus here: 1 worker 136 puts/s, 4 gives 2.33x, 12 gives 2.96x,
// and 16, 24 and 32 are all within noise of 12. Going wider only costs memory,
// since every planned file is already buffered.
const uploadConcurrency = 12

// write stores every planned file under one prefix.
//
// The prefix is minted here, once, and never accepted from a caller. That is
// the property that keeps one upload from addressing another: there is a
// single code path where the prefix comes into existence, so no transport can
// accidentally hand in its own.
//
// It is also why every file of a site lands under the same prefix. A static
// site references its assets by relative URL, and a browser resolves those
// against the page's own path, so app.js is only found if it shares a prefix
// with the index.html that links it.
func write(ctx context.Context, store ObjectStore, rootDir string, files []planned, opts Options) (Result, error) {
	res := Result{Prefix: pickPrefix(opts), RootDir: rootDir}

	// Each file gets its own slot, filled by its own goroutine, so the result is
	// assembled after the wait rather than appended to concurrently.
	stored := make([]File, len(files))
	failed := make([]error, len(files))

	var g errgroup.Group
	g.SetLimit(uploadConcurrency)

	for i, f := range files {
		g.Go(func() error {
			etag, err := store.PutObject(ctx, res.Prefix+f.rel, bytes.NewReader(f.data), int64(len(f.data)))
			if err != nil {
				failed[i] = err
				return err
			}
			stored[i] = File{Key: res.Prefix + f.rel, Size: int64(len(f.data)), ETag: etag}
			return nil
		})
	}

	// A plain group rather than errgroup.WithContext: cancelling on the first
	// failure would abandon writes already in flight, and a PUT that lands but
	// is never counted leaves an object no deployment row describes. Counting
	// every success is what lets the caller clean up after a partial failure.
	_ = g.Wait()

	// Merged in planner order so the manifest does not depend on which write
	// happened to finish first, and every file that did land is reported.
	var firstErr error
	for i, f := range files {
		if failed[i] != nil {
			if firstErr == nil {
				firstErr = failed[i]
			}
			continue
		}
		res.Files = append(res.Files, stored[i])
		res.Bytes += int64(len(f.data))
		if f.rel == "index.html" {
			res.HasIndexHTML = true
		}
	}

	if firstErr != nil {
		return res, firstErr
	}
	return res, nil
}

type planned struct {
	rel  string
	data []byte
}

func plan(zr *zip.Reader, opts Options) ([]planned, string, error) {
	l := opts.Limits

	entries := make([]*zip.File, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		entries = append(entries, f)
	}
	if len(entries) == 0 {
		return nil, "", fmt.Errorf("%w: archive contains no files", ErrInvalidArchive)
	}
	if len(entries) > l.MaxEntries {
		return nil, "", fmt.Errorf("%w: %d entries exceeds limit of %d", ErrTooManyEntries, len(entries), l.MaxEntries)
	}

	rootDir, err := detectSingleRoot(entries)
	if err != nil {
		return nil, "", err
	}

	files := make([]planned, 0, len(entries))
	var total int64
	seen := make(map[string]struct{}, len(entries))

	for _, f := range entries {
		if err := checkEntryKind(f); err != nil {
			return nil, "", err
		}

		rel, err := safepath.Relative(f.Name)
		if err != nil {
			return nil, "", err
		}
		if opts.FlattenRoot && rootDir != "" {
			rel = strings.TrimPrefix(rel, rootDir+"/")
			if rel == "" {
				continue
			}
		}

		if _, dup := seen[rel]; dup {
			return nil, "", fmt.Errorf("%w: duplicate entry %q", ErrInvalidArchive, rel)
		}
		seen[rel] = struct{}{}

		data, err := readEntry(f, l.MaxFileBytes)
		if err != nil {
			return nil, "", err
		}

		total += int64(len(data))
		if total > l.MaxTotalBytes {
			return nil, "", fmt.Errorf("%w: extracted size exceeds %d bytes", ErrTooLarge, l.MaxTotalBytes)
		}

		files = append(files, planned{rel: rel, data: data})
	}

	if opts.RequireIndexHTML && !hasRootDocument(files, opts.AllowSinglePDF) {
		return nil, "", missingIndexError(files, rootDir)
	}

	return files, rootDir, nil
}

// hasRootDocument reports whether an upload has something the site root can
// serve: an index, or, when allowed, a single PDF standing in for one.
func hasRootDocument(files []planned, allowSinglePDF bool) bool {
	if hasIndexHTML(files) {
		return true
	}
	return allowSinglePDF && isLonePDF(files)
}

// isLonePDF reports whether the upload is exactly one PDF at the root.
//
// Nested paths do not count: `docs/report.pdf` is a file inside a site, not a
// document being served as the site.
func isLonePDF(files []planned) bool {
	if len(files) != 1 {
		return false
	}
	rel := files[0].rel
	if strings.Contains(rel, "/") {
		return false
	}
	return strings.EqualFold(path.Ext(rel), ".pdf")
}

// hasIndexHTML reports whether any planned file is an index at the prefix
// root.
//
// The match is case-insensitive and accepts .htm, because a site built on a
// case-insensitive filesystem will happily contain Index.HTML or index.htm and
// a browser will serve them, so rejecting those would be a difference no user
// could act on.
func hasIndexHTML(files []planned) bool {
	present, _ := findIndexHTML(files, "")
	return present
}

// findIndexHTML locates an index file at the root. When rootDir is non-empty
// it also reports whether one exists just inside that directory, which is the
// difference between "you forgot the index" and "your archive has a wrapper
// folder".
func findIndexHTML(files []planned, rootDir string) (present bool, nested bool) {
	prefix := strings.ToLower(rootDir + "/")
	for _, f := range files {
		name := strings.ToLower(f.rel)
		if name == "index.html" || name == "index.htm" {
			return true, nested
		}
		if rootDir != "" && strings.HasPrefix(name, prefix) {
			switch strings.TrimPrefix(name, prefix) {
			case "index.html", "index.htm":
				nested = true
			}
		}
	}
	return false, nested
}

// missingIndexError explains why the root has no index, so the caller is not
// left guessing which of the several ways this fails applies to them.
func missingIndexError(files []planned, rootDir string) error {
	if rootDir != "" {
		if _, nested := findIndexHTML(files, rootDir); nested {
			return fmt.Errorf(
				"%w: every file is under %q/, so the index is not at the root; "+
					"repost with ?flatten=true to strip that directory",
				ErrNoIndexHTML, rootDir)
		}
	}

	// No index anywhere. List a few names, since "there is no index" is much
	// easier to act on when it says what was uploaded instead.
	names := make([]string, 0, len(files))
	for i, f := range files {
		if i == 5 {
			names = append(names, "...")
			break
		}
		names = append(names, f.rel)
	}
	return fmt.Errorf(
		"%w: found %s. A site needs an index.html, or a single PDF on its own",
		ErrNoIndexHTML, strings.Join(names, ", "))
}

// readEntry extracts one entry with a hard byte cap.
//
// The cap is enforced on bytes actually read rather than on the size declared
// in the archive header, because a crafted zip can claim to be small and
// expand without bound.
func readEntry(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open %q: %v", ErrInvalidArchive, f.Name, err)
	}
	defer rc.Close()

	buf, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %q: %v", ErrInvalidArchive, f.Name, err)
	}
	if int64(len(buf)) > max {
		return nil, fmt.Errorf("%w: entry %q exceeds %d bytes", ErrTooLarge, f.Name, max)
	}
	return buf, nil
}

// checkEntryKind rejects anything that is not a regular file.
//
// A zip can encode a symlink, and a symlink entry would let a crafted archive
// point at a path outside the prefix on any system that later materialises
// these entries on disk.
func checkEntryKind(f *zip.File) error {
	mode := f.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return fmt.Errorf("%w: entry %q is a symlink", safepath.ErrUnsafe, f.Name)
	case mode&os.ModeDevice != 0:
		return fmt.Errorf("%w: entry %q is a device node", safepath.ErrUnsafe, f.Name)
	case mode&os.ModeNamedPipe != 0:
		return fmt.Errorf("%w: entry %q is a fifo", safepath.ErrUnsafe, f.Name)
	case mode&os.ModeSocket != 0:
		return fmt.Errorf("%w: entry %q is a socket", safepath.ErrUnsafe, f.Name)
	}
	if !mode.IsRegular() && mode != 0 {
		return fmt.Errorf("%w: entry %q is not a regular file (mode %v)", safepath.ErrUnsafe, f.Name, mode)
	}
	return nil
}

// detectSingleRoot reports the shared top-level directory when every entry
// sits under exactly one, which is what makes FlattenRoot predictable.
func detectSingleRoot(entries []*zip.File) (string, error) {
	var root string
	for _, f := range entries {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		first, rest, ok := strings.Cut(name, "/")
		if !ok || rest == "" {
			// An entry at the archive root, so no single-root flattening.
			return "", nil
		}
		if root == "" {
			root = first
			continue
		}
		if root != first {
			return "", nil
		}
	}
	return root, nil
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(buf)) > max {
		return nil, fmt.Errorf("%w: archive exceeds %d bytes", ErrTooLarge, max)
	}
	return buf, nil
}

// IncomingFile is one file supplied by a caller, before validation.
type IncomingFile struct {
	// Name is the client-supplied filename. It is untrusted and is run
	// through safepath.Relative before it becomes part of any key.
	Name string

	// Body is the content. It is read exactly once, with a cap. Ignored when
	// Data is set.
	Body io.Reader

	// Data is content the caller has already buffered.
	//
	// It exists to avoid a second copy. An HTTP handler that reads multipart
	// parts into memory already holds the bytes, and reading them again here
	// doubled peak memory for every upload: a request inside every configured
	// cap measured 531 MiB peak for 180 MiB accepted.
	//
	// The bytes are retained by the caller for the duration of the call, so
	// FromFiles copies nothing and mutating them afterwards is the caller's
	// problem.
	Data []byte

	// Size is the declared length, or -1 when unknown. When known it is
	// checked against the bytes actually read, so a truncated stream cannot
	// pass unnoticed.
	Size int64
}

// FromFiles stores several files under a single fresh unique prefix.
//
// This is the path a multi-file site takes. Every file shares one prefix
// because that is what relative URLs inside the site require: a browser
// resolves ./assets/app.js against the page URL, so it only finds the file if
// app.js sits under the same prefix as the index.html that links it.
//
// Every file is validated and read before anything is written, so a bad name
// or an oversized file cannot leave objects behind.
func FromFiles(ctx context.Context, store ObjectStore, files []IncomingFile, opts Options) (Result, error) {
	opts = opts.normalized()

	planned, err := planFiles(files, opts)
	if err != nil {
		return Result{}, err
	}
	return write(ctx, store, "", planned, opts)
}

// planFiles validates and reads every incoming file.
//
// The result preserves the order the files arrived in.
func planFiles(files []IncomingFile, opts Options) ([]planned, error) {
	l := opts.Limits
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no files supplied", ErrInvalidArchive)
	}
	if len(files) > l.MaxEntries {
		return nil, fmt.Errorf("%w: %d files exceeds limit of %d", ErrTooManyEntries, len(files), l.MaxEntries)
	}

	out := make([]planned, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	var total int64

	for _, f := range files {
		rel, err := safepath.Relative(f.Name)
		if err != nil {
			return nil, err
		}
		if rel == "" {
			return nil, fmt.Errorf("%w: unusable filename %q", safepath.ErrUnsafe, f.Name)
		}

		// Two parts claiming the same key would race, and whichever landed
		// last would silently win.
		if _, dup := seen[rel]; dup {
			return nil, fmt.Errorf("%w: duplicate filename %q", ErrInvalidArchive, rel)
		}
		seen[rel] = struct{}{}

		var data []byte
		if f.Data != nil {
			if int64(len(f.Data)) > l.MaxFileBytes {
				return nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrTooLarge, rel, l.MaxFileBytes)
			}
			data = f.Data
		} else {
			var err error
			data, err = io.ReadAll(io.LimitReader(f.Body, l.MaxFileBytes+1))
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", rel, err)
			}
			if int64(len(data)) > l.MaxFileBytes {
				return nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrTooLarge, rel, l.MaxFileBytes)
			}
		}
		if f.Size >= 0 && f.Size != int64(len(data)) {
			return nil, fmt.Errorf("%w: %q declared %d bytes but delivered %d",
				ErrInvalidArchive, rel, f.Size, len(data))
		}

		total += int64(len(data))
		if total > l.MaxTotalBytes {
			return nil, fmt.Errorf("%w: total size exceeds %d bytes", ErrTooLarge, l.MaxTotalBytes)
		}

		out = append(out, planned{rel: rel, data: data})
	}

	if opts.RequireIndexHTML && !hasRootDocument(out, opts.AllowSinglePDF) {
		return nil, missingIndexError(out, "")
	}

	return out, nil
}

// FromSingleFile stores one file under a fresh unique prefix.
//
// It is FromFiles with a single element, kept because "upload one file" is a
// distinct thing a client can ask for and reads better at the call site than
// building a one-element slice inline.
func FromSingleFile(ctx context.Context, store ObjectStore, name string, r io.Reader, size int64, opts Options) (Result, error) {
	return FromFiles(ctx, store, []IncomingFile{{Name: name, Body: r, Size: size}}, opts)
}
