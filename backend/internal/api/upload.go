// Package api is the HTTP transport.
//
// Handlers translate a request into calls on the upload and database packages
// and translate the result back. There is no business logic here: anything
// worth testing belongs in those packages.
package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/safepath"
	"github.com/dk10ws/ironhide/internal/upload"
)

// zipMagic is the local file header every zip archive starts with.
//
// Detection is by content, never by filename or the client's Content-Type,
// because both are attacker-controlled: a caller can name a bomb "index.html".
var zipMagic = []byte{'P', 'K', 0x03, 0x04}

// maxParts bounds how many multipart parts one request may declare, so a
// request cannot make the runtime build a huge slice of headers before the
// extract caps get a chance to reject anything.
const maxParts = 8192

func (h *Handler) uploadZip(c *gin.Context) {
	parts, err := readParts(c, h.cfg.Upload.MaxBytes)
	if err != nil {
		h.fail(c, err)
		return
	}
	// Arity is checked before anything is buffered, so a client sending many
	// large parts is refused without the memory ever being allocated.
	switch len(parts) {
	case 0:
		h.fail(c, fmt.Errorf("%w: no file part supplied", upload.ErrInvalidArchive))
		return
	case 1:
	default:
		h.fail(c, fmt.Errorf("%w: send exactly one zip archive, got %d parts",
			upload.ErrInvalidArchive, len(parts)))
		return
	}

	raw := parts[0].data
	if len(raw) > int(h.cfg.Upload.ArchiveBytes) {
		h.fail(c, fmt.Errorf("%w: archive exceeds %d bytes", upload.ErrTooLarge, h.cfg.Upload.ArchiveBytes))
		return
	}

	if !bytes.HasPrefix(raw, zipMagic) {
		h.fail(c, fmt.Errorf("%w: body does not start with a zip header", upload.ErrInvalidArchive))
		return
	}

	opts := upload.Options{
		Limits:           h.cfg.Upload.Limits(),
		FlattenRoot:      c.Query("flatten") == "true",
		RequireIndexHTML: true,
	}

	res, err := upload.FromZip(c.Request.Context(), h.store, bytes.NewReader(raw), opts)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.respondCreated(c, res)
}

func (h *Handler) uploadFile(c *gin.Context) {
	parts, err := readParts(c, h.cfg.Upload.MaxBytes)
	if err != nil {
		h.fail(c, err)
		return
	}
	if len(parts) != 1 {
		h.fail(c, fmt.Errorf("%w: send exactly one file, got %d", upload.ErrInvalidArchive, len(parts)))
		return
	}

	res, err := upload.FromSingleFile(c.Request.Context(), h.store,
		parts[0].name, bytes.NewReader(parts[0].data), int64(len(parts[0].data)), upload.Options{
			Limits:           h.cfg.Upload.Limits(),
			RequireIndexHTML: true,
		})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.respondCreated(c, res)
}

func (h *Handler) uploadFiles(c *gin.Context) {
	parts, err := readParts(c, h.cfg.Upload.MaxBytes)
	if err != nil {
		h.fail(c, err)
		return
	}
	if len(parts) == 0 {
		h.fail(c, fmt.Errorf("%w: no file parts supplied", upload.ErrInvalidArchive))
		return
	}

	files := make([]upload.IncomingFile, 0, len(parts))
	for _, p := range parts {
		files = append(files, upload.IncomingFile{
			Name: p.name,
			Data: p.data,
			Size: int64(len(p.data)),
		})
	}

	res, err := upload.FromFiles(c.Request.Context(), h.store, files, upload.Options{
		Limits:           h.cfg.Upload.Limits(),
		RequireIndexHTML: true,
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.respondCreated(c, res)
}

// part is one file part, already read into memory under the request cap.
type part struct {
	name string
	data []byte
	ct   string
}

// readParts parses a multipart body into parts held in memory.
//
// It does not use gin.Context.MultipartForm: that buffers small parts in
// memory and spills large ones to a temp file, which cannot work because the
// image is built FROM scratch with no writable filesystem.
func readParts(c *gin.Context, maxBytes int64) ([]part, error) {
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return nil, fmt.Errorf("%w: expected a multipart/form-data body", upload.ErrInvalidArchive)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("%w: multipart boundary missing", upload.ErrInvalidArchive)
	}

	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	mr := multipart.NewReader(body, boundary)

	var parts []part
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, parseErr(err)
		}

		// Only parts that look like files are collected; a plain form field
		// has no filename and is drained so the stream stays in sync.
		if partFilename(p) == "" {
			_, _ = io.Copy(io.Discard, io.LimitReader(p, 1<<20))
			p.Close()
			continue
		}

		if len(parts) >= maxParts {
			p.Close()
			return nil, fmt.Errorf("%w: more than %d file parts", upload.ErrTooManyEntries, maxParts)
		}

		data, err := io.ReadAll(p)
		name := partFilename(p)
		p.Close()
		if err != nil {
			return nil, parseErr(err)
		}

		parts = append(parts, part{name: name, data: data, ct: p.Header.Get("Content-Type")})
	}

	return parts, nil
}

// partFilename returns the filename exactly as the client sent it, including
// any directory part.
//
// multipart.Part.FileName is deliberately not used: it applies filepath.Base,
// which is the right default for a web form but destroys the structure of a
// site. Zipping a project and posting the members individually is the common
// case for a scripted client, and flattening assets/app.js next to index.html
// breaks every relative URL in the page.
//
// Safety does not depend on the base-name stripping, because
// safepath.Relative runs over this value on the way in and rejects traversal,
// absolute paths, backslashes and drive letters.
func partFilename(p *multipart.Part) string {
	_, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	// filename* (RFC 5987) is already decoded by ParseMediaType into
	// "filename", so non-ASCII names arrive intact here.
	return params["filename"]
}

// parseErr maps a streaming failure onto an upload error.
func parseErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return fmt.Errorf("%w: request body too large", upload.ErrTooLarge)
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: truncated multipart body", upload.ErrInvalidArchive)
	}
	return fmt.Errorf("%w: malformed multipart body: %v", upload.ErrInvalidArchive, err)
}

// respondCreated records the deployment and returns the drop URLs.
func (h *Handler) respondCreated(c *gin.Context, res upload.Result) {
	entries := make([]database.ManifestEntry, 0, len(res.Files))
	for _, f := range res.Files {
		entries = append(entries, database.ManifestEntry{Key: f.Key, Size: f.Size, ETag: f.ETag})
	}

	created, err := h.db.CreateDeployment(c.Request.Context(), database.CreateDeploymentInput{
		Prefix: res.Prefix,
		Files:  entries,
		TTL:    h.cfg.Upload.TTL,
	})
	if err != nil {
		// The objects are already in the bucket. The row is missing, so the
		// sweeper cannot find these objects later; log the prefix loudly so
		// they are recoverable rather than silently orphaned.
		slog.Error("deployment insert failed, objects orphaned",
			"prefix", res.Prefix, "files", len(res.Files), "err", err)
		h.fail(c, err)
		return
	}

	d := created.Deployment
	slog.Info("upload accepted",
		"prefix", d.Prefix,
		"files", len(res.Files),
		"bytes", res.Bytes,
		"id", d.ID,
	)

	c.JSON(http.StatusCreated, gin.H{
		"id":          d.ID,
		"preview_url": h.previewURL(d.Slug()),
		"claim_url":   h.claimURL(created.ClaimToken),
		"expires_at":  d.ExpiresAt.Format(time.RFC3339),
		"bytes":       res.Bytes,
		"files":       res.Files,
	})
}

// claimURL builds the single-use claim link for a drop.
func (h *Handler) claimURL(token string) string {
	return h.cfg.PublicURL + "/claim/" + token
}

// fail maps an error onto a status code and a message safe to return.
func (h *Handler) fail(c *gin.Context, err error) {
	code := h.statusFor(err)

	msg := err.Error()
	if code >= 500 {
		slog.Error("request failed", "path", c.Request.URL.Path, "err", err)
		// Do not leak driver detail to an anonymous caller.
		msg = "internal error"
	}

	c.JSON(code, gin.H{"error": msg})
}

// statusFor maps an upload or storage error onto an HTTP status.
func (h *Handler) statusFor(err error) int {
	switch {
	case errors.Is(err, upload.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, upload.ErrTooManyEntries):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, upload.ErrInvalidArchive):
		return http.StatusBadRequest
	case errors.Is(err, upload.ErrNoIndexHTML):
		return http.StatusUnprocessableEntity
	case errors.Is(err, safepath.ErrUnsafe):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
