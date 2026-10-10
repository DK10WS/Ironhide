// Package api is the HTTP transport.
//
// Handlers translate a request into calls on the upload and database packages
// and translate the result back. There is no business logic here: anything
// worth testing belongs in those packages.
package api

import (
	"bytes"
	"context"
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
	h.uploadZipTo(c, target{})
}

func (h *Handler) uploadZipTo(c *gin.Context, to target) {
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

	// Checked before anything is stored. The archive is compressed, so this
	// bounds the request rather than the result; the real total is re-checked
	// against the quota once the manifest is known, before the row goes live.
	if to.owned() && !h.withinQuota(c, to, int64(len(raw))) {
		return
	}

	opts := upload.Options{
		Limits:           h.cfg.Upload.Limits(),
		FlattenRoot:      c.Query("flatten") == "true",
		RequireIndexHTML: true,
		AllowSinglePDF:   true,
	}

	h.storeUpload(c, to, func(prefix string) (upload.Result, error) {
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

	if !h.withinQuota(c, target{}, int64(len(parts[0].data))) {
		return
	}

	h.storeUpload(c, target{}, func(prefix string) (upload.Result, error) {
		return upload.FromSingleFile(c.Request.Context(), h.store,
			parts[0].name, bytes.NewReader(parts[0].data), int64(len(parts[0].data)), upload.Options{
				Limits:           h.cfg.Upload.Limits(),
				RequireIndexHTML: true,
				AllowSinglePDF:   true,
				Prefix:           prefix,
			})
	})
}

func (h *Handler) uploadFiles(c *gin.Context) {
	h.uploadFilesTo(c, target{})
}

func (h *Handler) uploadFilesTo(c *gin.Context, to target) {
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
	var total int64
	for _, p := range parts {
		files = append(files, upload.IncomingFile{
			Name: p.name,
			Data: p.data,
			Size: int64(len(p.data)),
		})
		total += int64(len(p.data))
	}

	// Uncompressed, so this is the figure the quota is actually measured in.
	if to.owned() && !h.withinQuota(c, to, total) {
		return
	}

	h.storeUpload(c, to, func(prefix string) (upload.Result, error) {
		return upload.FromFiles(c.Request.Context(), h.store, files, upload.Options{
			Limits:           h.cfg.Upload.Limits(),
			RequireIndexHTML: true,
			AllowSinglePDF:   true,
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
// target says who an upload is for. The zero value is the pre-existing
// behaviour: an anonymous temporary drop that expires and is kept alive by a
// claim link.
type target struct {
	// ownerID is Google's subject id. Empty means anonymous.
	ownerID string

	// siteID is the site being deployed to. Zero means this upload creates a
	// new site rather than replacing one.
	siteID uint

	// prefix reuses an existing prefix on a redeploy, which is what keeps the
	// address stable. Empty means mint a fresh one.
	prefix string
}

// owned reports whether this upload belongs to an account.
func (t target) owned() bool { return t.ownerID != "" }

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
func (h *Handler) storeUpload(c *gin.Context, to target, store func(prefix string) (upload.Result, error)) {
	ctx := c.Request.Context()

	// A redeploy writes over the site's existing prefix so its address does
	// not change. Otherwise a fresh one is minted here rather than inside
	// upload so the row can carry it. It is still server-generated: nothing
	// about it comes from the request, which is what keeps one upload from
	// addressing another.
	prefix := to.prefix
	if prefix == "" {
		prefix = upload.NewPrefix()
	}

	input := database.CreateDeploymentInput{Prefix: prefix}

	if to.owned() {
		// An owned deployment is not a drop: it has no claim window, because
		// there is nothing to claim and nothing to expire.
		//
		// ClaimedBy is deliberately left empty here. Setting it would make
		// CreateDeployment write the row live immediately, and RecordUpload
		// below exists to promote a pending row once the objects are really
		// there. The ownership is recorded by attachOwned, after the write.
		input.OwnerID = to.ownerID
		input.TTL = 0
	} else {
		input.TTL = h.cfg.Upload.TTL
	}

	created, err := h.db.CreateDeployment(ctx, input)
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

	if err := h.db.RecordUpload(ctx, created.Deployment.ID, res); err != nil {
		// The objects are stored but the manifest could not be written, so
		// the row cannot describe them. Fail it rather than serving a site
		// whose recorded contents disagree with the bucket.
		h.abandonUpload(c, created.Deployment.ID, res, err)
		return
	}

	slug := created.Deployment.Slug()

	if to.owned() {
		if err := h.attachOwned(ctx, to, created.Deployment.ID, slug, res); err != nil {
			// The objects exist but nothing ties them to the account, so the
			// row has to go rather than leave bytes nobody can find.
			h.abandonUpload(c, created.Deployment.ID, res, err)
			return
		}
	}

	slog.Info("upload accepted",
		"id", created.Deployment.ID,
		"prefix", created.Deployment.Prefix,
		"files", len(res.Files),
		"bytes", res.Bytes,
		"owned", to.owned(),
	)

	body := gin.H{
		"id":          created.Deployment.ID,
		"preview_url": h.previewURL(slug),
		"expires_at":  expiresAt(created.Deployment),
		"bytes":       res.Bytes,
		"files":       res.Files,
	}

	if to.owned() {
		// No claim link: the site is already permanent and owned, so there is
		// nothing to redeem and no credential worth putting in a response.
		body["owned"] = true
	} else {
		body["claim_url"] = h.claimURL(created.ClaimToken)
	}

	c.JSON(http.StatusCreated, body)
}

// attachOwned ties a finished upload to a site in the owner's account.
//
// A new site takes its name from the deployment's slug, which is the label the
// address is built from. A redeploy keeps the existing row so the address is
// unchanged, and retires the deployment it supersedes so the old bytes stop
// counting against the quota in the same transaction.
func (h *Handler) attachOwned(ctx context.Context, to target, deploymentID uint, slug string, res upload.Result) error {
	if to.siteID != 0 {
		if err := h.db.AttachSiteToDeployment(ctx, deploymentID, to.siteID, to.ownerID); err != nil {
			return err
		}
		// A redeploy wrote over the old prefix, so anything the new manifest no
		// longer lists is now unreachable and still counted against the quota.
		return h.deleteStaleKeys(ctx, to.siteID, res.Files)
	}

	site, err := h.db.UpsertSite(ctx, slug, strings.TrimPrefix(h.previewURL(slug), h.cfg.DataScheme+"://"), to.ownerID)
	if err != nil {
		return err
	}
	return h.db.AttachSiteToDeployment(ctx, deploymentID, site.ID, to.ownerID)
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
