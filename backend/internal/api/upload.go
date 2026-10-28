// Package api is the HTTP transport.
//
// Handlers translate a request into a call on package core and translate the
// result back. There is no business logic here: anything worth testing belongs
// in core, so the MCP transport can share it.
package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/core"
	"github.com/dk10ws/ironhide/internal/safepath"
	"github.com/dk10ws/ironhide/internal/upload"
)

// maxParts bounds how many multipart parts one request may declare, so a
// request cannot make the runtime build a huge slice of headers before the
// extract caps get a chance to reject anything.
const maxParts = 8192

func (h *Handler) uploadZip(c *gin.Context) {
	h.uploadZipTo(c, core.Target{})
}

func (h *Handler) uploadZipTo(c *gin.Context, to core.Target) {
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

	res, err := h.core.Upload(c.Request.Context(), to, core.Payload{
		Zip:     parts[0].data,
		Flatten: c.Query("flatten") == "true",
	})
	if err != nil {
		h.fail(c, err)
		return
	}

	c.JSON(http.StatusCreated, uploadBody(res))
}

// uploadFile stores a single file.
//
// It stays anonymous regardless of the caller's session: this is the route a
// script posts one page to, and it has never created an owned site.
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

	res, err := h.core.Upload(c.Request.Context(), core.Target{}, core.Payload{
		Files: []upload.IncomingFile{{
			Name: parts[0].name,
			Data: parts[0].data,
			Size: int64(len(parts[0].data)),
		}},
	})
	if err != nil {
		h.fail(c, err)
		return
	}

	c.JSON(http.StatusCreated, uploadBody(res))
}

func (h *Handler) uploadFiles(c *gin.Context) {
	h.uploadFilesTo(c, core.Target{})
}

func (h *Handler) uploadFilesTo(c *gin.Context, to core.Target) {
	parts, err := readParts(c, h.cfg.Upload.MaxBytes)
	if err != nil {
		h.fail(c, err)
		return
	}
	if len(parts) == 0 {
		h.fail(c, fmt.Errorf("%w: no file parts supplied", upload.ErrInvalidArchive))
		return
	}

	res, err := h.core.Upload(c.Request.Context(), to, core.Payload{
		Files: incomingFiles(parts),
	})
	if err != nil {
		h.fail(c, err)
		return
	}

	c.JSON(http.StatusCreated, uploadBody(res))
}

// incomingFiles converts parsed parts into the form core and upload take.
func incomingFiles(parts []part) []upload.IncomingFile {
	files := make([]upload.IncomingFile, 0, len(parts))
	for _, p := range parts {
		files = append(files, upload.IncomingFile{
			Name: p.name,
			Data: p.data,
			Size: int64(len(p.data)),
		})
	}
	return files
}

// uploadBody renders an upload result as the JSON clients already expect.
func uploadBody(res core.Result) gin.H {
	body := gin.H{
		"id":          res.ID,
		"preview_url": res.PreviewURL,
		"expires_at":  expiresAt(res.ExpiresAt),
		"bytes":       res.Bytes,
		"files":       res.Files,
	}

	if res.Owned {
		// No claim link: the site is already permanent and owned, so there is
		// nothing to redeem and no credential worth putting in a response.
		body["owned"] = true
	} else {
		body["claim_url"] = res.ClaimURL
	}

	return body
}

// expiresAt renders the claim deadline, or null when the deployment is
// permanent. A permanent drop has no deadline to report, and emitting the zero
// time would be a lie.
func expiresAt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
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

// fail maps an error onto a status code and a message safe to return.
func (h *Handler) fail(c *gin.Context, err error) {
	code := h.statusFor(err)

	msg := err.Error()
	if errors.Is(err, upload.ErrTooLarge) {
		// The underlying errors carry the raw limit, which is the right thing
		// to log but nonsense to read: "exceeds 52428800 bytes". Say it as a
		// size instead, keeping whatever named the offending file.
		msg = humanSize(msg)
	}
	if code >= 500 {
		slog.Error("request failed", "path", c.Request.URL.Path, "err", err)
		// Do not leak driver detail to an anonymous caller.
		msg = "internal error"
	}

	c.JSON(code, gin.H{"error": msg})
}

// humanSize rewrites a trailing "N bytes" limit as a rounded size.
//
// The number is scanned backwards off the end rather than matched with a
// pattern, because everything before it is a file name that may itself contain
// digits, spaces and quotes.
func humanSize(msg string) string {
	const suffix = " bytes"
	end := strings.LastIndex(msg, suffix)
	if end < 0 {
		return msg
	}

	// Walk back over the digits to find where the limit starts.
	start := end
	for start > 0 && msg[start-1] >= '0' && msg[start-1] <= '9' {
		start--
	}
	if start == end {
		return msg
	}

	n, err := strconv.ParseInt(msg[start:end], 10, 64)
	if err != nil {
		return msg
	}
	return msg[:start] + formatSize(n)
}

// formatSize renders a byte count the way a person would say it.
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 3; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// statusFor maps an upload or storage error onto an HTTP status.
func (h *Handler) statusFor(err error) int {
	switch {
	case errors.Is(err, core.ErrQuota):
		return http.StatusRequestEntityTooLarge
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
