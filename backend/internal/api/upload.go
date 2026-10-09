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

	h.storeUpload(c, func(prefix string) (upload.Result, error) {
		opts.Prefix = prefix
		return upload.FromZip(c.Request.Context(), h.store, bytes.NewReader(raw), opts)
	})
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

	h.storeUpload(c, func(prefix string) (upload.Result, error) {
		return upload.FromSingleFile(c.Request.Context(), h.store,
			parts[0].name, bytes.NewReader(parts[0].data), int64(len(parts[0].data)), upload.Options{
				Limits:           h.cfg.Upload.Limits(),
				RequireIndexHTML: true,
				Prefix:           prefix,
			})
	})
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

	h.storeUpload(c, func(prefix string) (upload.Result, error) {
		return upload.FromFiles(c.Request.Context(), h.store, files, upload.Options{
			Limits:           h.cfg.Upload.Limits(),
			RequireIndexHTML: true,
			Prefix:           prefix,
		})
	})
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

// storeUpload runs the three-step write for every upload shape.
//
// The order is the point:
//
//  1. create a deployment row, still pending, carrying the object prefix
//  2. write the objects under that prefix
//  3. promote the row to live
//
// Because the row is written first, bytes can never exist without a row
// describing them. The failure mode is the reverse — a pending row with no or
// partial objects — and that direction is the cheap one to clean up, because
// Postgres can find it with an indexed query while orphaned objects would
// require listing every key in the bucket.
func (h *Handler) storeUpload(c *gin.Context, store func(prefix string) (upload.Result, error)) {
	// The prefix is generated here rather than inside upload so the row can
	// carry it. It is still server-generated: nothing about it comes from the
	// request, which is what keeps one upload from addressing another.
	prefix := upload.NewPrefix()

	created, err := h.db.CreateDeployment(c.Request.Context(), database.CreateDeploymentInput{
		Prefix: prefix,
		TTL:    h.cfg.Upload.TTL,
	})
	if err != nil {
		h.fail(c, err)
		return
	}

	// No files are known yet, so the row starts empty and MarkLive fills in
	// the real counts from the upload result.
	res, err := store(prefix)
	if err != nil {
		h.abandonUpload(c, created.Deployment.ID, res, err)
		return
	}

	if err := h.db.RecordUpload(c.Request.Context(), created.Deployment.ID, res); err != nil {
		// The objects are stored but the manifest could not be written, so
		// the row cannot describe them. Fail it rather than serving a site
		// whose recorded contents disagree with the bucket.
		h.abandonUpload(c, created.Deployment.ID, res, err)
		return
	}

	slog.Info("upload accepted",
		"id", created.Deployment.ID,
		"prefix", created.Deployment.Prefix,
		"files", len(res.Files),
		"bytes", res.Bytes,
	)

	c.JSON(http.StatusCreated, gin.H{
		"id":          created.Deployment.ID,
		"preview_url": h.previewURL(created.Deployment.Slug()),
		"claim_url":   h.claimURL(created.ClaimToken),
		"expires_at":  expiresAt(created.Deployment),
		"bytes":       res.Bytes,
		"files":       res.Files,
	})
}

// abandonUpload deals with the row for an upload that did not finish.
//
// If nothing was written the row is deleted outright, because a row exists to
// describe objects and there are none. Keeping a failed row per rejected
// request would let an unauthenticated caller fill the deployments table for
// free, and almost every rejection is a validation failure that never touched
// the bucket.
//
// If objects did land, the row is marked failed instead: the reconciler will
// report the prefix so whatever was written can be found, and nothing is lost.
func (h *Handler) abandonUpload(c *gin.Context, id uint, res upload.Result, cause error) {
	ctx := c.Request.Context()

	var cleanupErr error
	if len(res.Files) == 0 {
		cleanupErr = h.db.DeleteDeployment(ctx, id)
	} else {
		cleanupErr = h.db.MarkFailed(ctx, id)
	}
	if cleanupErr != nil {
		slog.Error("could not clean up a failed upload",
			"id", id, "prefix", res.Prefix, "err", cleanupErr)
	}

	h.fail(c, cause)
}

// expiresAt renders the claim deadline, or null when the deployment is
// permanent. A permanent drop has no deadline to report, and emitting the zero
// time would be a lie.
func expiresAt(d *database.Deployment) any {
	if d.ExpiresAt == nil {
		return nil
	}
	return d.ExpiresAt.Format(time.RFC3339)
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
