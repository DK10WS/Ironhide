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
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dk10ws/ironhide/internal/safepath"
)

type ObjectStore interface {
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) (string, error)
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
		MaxArchiveBytes: 128 << 20, // 128 MiB compressed
		MaxFileBytes:    32 << 20,  // 32 MiB per file
		MaxTotalBytes:   256 << 20, // 256 MiB uncompressed
		MaxEntries:      4096,
	}
}

type Options struct {
	Limits Limits

	FlattenRoot bool
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
	return Options{Limits: l, FlattenRoot: o.FlattenRoot}
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

	res := Result{Prefix: newPrefix(), RootDir: rootDir}
	if rootDir == "" {
		res.Prefix = newPrefix()
	}

	for _, f := range files {
		etag, err := store.PutObject(ctx, res.Prefix+f.rel, bytes.NewReader(f.data), int64(len(f.data)), f.contentType)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, File{Key: res.Prefix + f.rel, Size: int64(len(f.data)), ETag: etag})
		res.Bytes += int64(len(f.data))
		if f.rel == "index.html" {
			res.HasIndexHTML = true
		}
	}

	return res, nil
}

type planned struct {
	rel         string
	contentType string
	data        []byte
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

		files = append(files, planned{rel: rel, contentType: ContentTypeFor(rel), data: data})
	}

	return files, rootDir, nil
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

// FromSingleFile stores one file under a fresh unique prefix.
//
// Unlike an archive, a single file has no internal name, so name is required.
// It is validated exactly like a zip member: a client-supplied filename is
// attacker-controlled and must not be able to address anything outside the new
// prefix.
func FromSingleFile(ctx context.Context, store ObjectStore, name string, r io.Reader, size int64, opts Options) (Result, error) {
	opts = opts.normalized()

	rel, err := safepath.Relative(name)
	if err != nil {
		return Result{}, err
	}
	if rel == "" {
		return Result{}, fmt.Errorf("%w: unusable filename", safepath.ErrUnsafe)
	}

	data, err := io.ReadAll(io.LimitReader(r, opts.Limits.MaxFileBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(data)) > opts.Limits.MaxFileBytes {
		return Result{}, fmt.Errorf("%w: file exceeds %d bytes", ErrTooLarge, opts.Limits.MaxFileBytes)
	}
	if size >= 0 && size != int64(len(data)) {
		return Result{}, fmt.Errorf("%w: declared size %d does not match %d bytes read", ErrInvalidArchive, size, len(data))
	}

	res := Result{Prefix: newPrefix()}
	etag, err := store.PutObject(ctx, res.Prefix+rel, bytes.NewReader(data), int64(len(data)), ContentTypeFor(rel))
	if err != nil {
		return res, err
	}

	res.Files = []File{{Key: res.Prefix + rel, Size: int64(len(data)), ETag: etag}}
	res.Bytes = int64(len(data))
	res.HasIndexHTML = rel == "index.html"
	return res, nil
}
