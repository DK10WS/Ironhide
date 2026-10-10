package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

// errQuota is what a caller sees when an upload would take them past their
// allowance. It is a 413 because the size of the request is the problem, not
// the request itself.
var errQuota = errors.New("storage quota exceeded")

// registerSites wires the account's own resources. Every route here requires a
// session, so none of them can be reached anonymously.
func (h *Handler) registerSites(r *gin.RouterGroup) {
	my := r.Group("/my", h.requireAccount)
	{
		my.GET("/sites", h.mySites)
		my.DELETE("/sites/:id", h.deleteSite)
		my.POST("/sites/:id/deploy", h.redeployToSite)
		my.GET("/usage", h.usage)
	}
}

func (h *Handler) ownedUploadZip(c *gin.Context) {
	h.uploadZipTo(c, h.targetFor(c))
}

func (h *Handler) ownedUploadFiles(c *gin.Context) {
	h.uploadFilesTo(c, h.targetFor(c))
}

// siteView is one row of the dashboard.
type siteView struct {
	ID   uint   `json:"id"`
	Slug string `json:"slug"`
	URL  string `json:"url"`

	Bytes      int64  `json:"bytes"`
	Files      int    `json:"files"`
	DeployedAt string `json:"deployed_at"`
	Live       bool   `json:"live"`
}

// mySites lists the signed-in account's sites.
func (h *Handler) mySites(c *gin.Context) {
	a := currentAccount(c)

	sites, err := h.db.ListOwnedSites(c.Request.Context(), a.User.ID)
	if err != nil {
		h.fail(c, err)
		return
	}

	out := make([]siteView, 0, len(sites))
	for _, s := range sites {
		view := siteView{
			ID:    s.ID,
			Slug:  s.Slug,
			URL:   h.previewURL(s.Slug),
			Bytes: s.Bytes,
			Files: s.Files,
			Live:  s.Live,
		}
		if s.DeployedAt != nil {
			view.DeployedAt = s.DeployedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, view)
	}

	c.JSON(http.StatusOK, gin.H{"sites": out})
}

// usage reports how much of the allowance is spent, so the dashboard can show
// the bar without recomputing it in the browser.
func (h *Handler) usage(c *gin.Context) {
	a := currentAccount(c)

	used, err := h.db.UsageBytes(c.Request.Context(), a.User.ID)
	if err != nil {
		h.fail(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"used":  used,
		"limit": h.cfg.Quota.BytesPerUser,
	})
}

// deleteSite removes a site from its owner's account.
//
// The objects are not deleted here. Garage has no single-call prefix delete and
// a large site can take many batched round trips, which would hold the request
// open far too long. Instead the site row goes and its deployments become
// anonymous drops, which the purger already knows how to destroy. The account
// stops being charged for them at the same moment, because owner_id is cleared
// in the same transaction.
func (h *Handler) deleteSite(c *gin.Context) {
	site, ok := h.ownedSite(c)
	if !ok {
		return
	}

	if err := h.db.ReleaseSite(c.Request.Context(), site.ID); err != nil {
		h.fail(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "id": site.ID})
}

// checkQuota refuses an upload that would take the account past its allowance.
//
// It runs before any bytes are written, because writing first and checking
// after would mean storing objects that are already over the limit. A redeploy
// that replaces the site's current deployment is credited back the bytes that
// deployment holds, since those are about to stop existing.
func (h *Handler) checkQuota(ctx ownerContext, incoming int64) error {
	limit := h.cfg.Quota.BytesPerUser
	if limit <= 0 {
		return nil
	}

	used, err := h.db.UsageBytes(ctx.ctx, ctx.ownerID)
	if err != nil {
		return err
	}

	credit := int64(0)
	if ctx.siteID != 0 {
		previous, err := h.db.UsageBySite(ctx.ctx, ctx.siteID)
		if err != nil {
			return err
		}
		credit = previous
	}

	if used-credit+incoming > limit {
		return fmt.Errorf("%w: this upload would use %d of %d bytes",
			errQuota, used-credit+incoming, limit)
	}
	return nil
}

// withinQuota reports whether an upload of this size is affordable, and answers
// the request with 413 when it is not.
//
// It runs before any object is written: checking afterwards would mean storing
// bytes that are already over the limit and then refusing to describe them.
func (h *Handler) withinQuota(c *gin.Context, to target, incoming int64) bool {
	if !to.owned() {
		return true
	}
	if err := h.checkQuota(ownerContext{ctx: c.Request.Context(), ownerID: to.ownerID, siteID: to.siteID}, incoming); err != nil {
		if errors.Is(err, errQuota) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
			return false
		}
		h.fail(c, err)
		return false
	}
	return true
}

// targetFor works out who an ordinary upload is for.
//
// A session turns the upload into an owned, permanent site with no claim link.
// Without one it stays a temporary drop, which is the behaviour that existed
// before accounts did and is still the only option for an anonymous caller.
func (h *Handler) targetFor(c *gin.Context) target {
	value, ok := c.Get(accountKey)
	if !ok {
		return target{}
	}
	a := value.(account)
	if a.User == nil {
		return target{}
	}
	return target{ownerID: a.User.ID}
}

// redeployToSite replaces the contents of a site the caller owns.
//
// The objects go under the site's existing prefix, so the address does not
// change; the deployment row is retired and a new one recorded, so the manifest
// and the bucket cannot drift apart.
func (h *Handler) redeployToSite(c *gin.Context) {
	a := currentAccount(c)
	ctx := c.Request.Context()

	site, ok := h.ownedSite(c)
	if !ok {
		return
	}

	// The prefix of the site's current deployment is the site's address. With
	// no deployment yet the site's slug is the whole of it.
	prefix := "uploads/" + site.Slug + "/"
	if dep, err := h.db.GetDeploymentBySite(ctx, site.ID); err == nil && dep != nil {
		prefix = dep.Prefix
	}

	to := target{ownerID: a.User.ID, siteID: site.ID, prefix: prefix}

	// One route covering both shapes: a single part that starts with a zip
	// header is an archive, anything else is the site itself. That keeps the
	// dashboard's redeploy to one button instead of three.
	parts, err := readParts(c, h.cfg.Upload.MaxBytes)
	if err != nil {
		h.fail(c, err)
		return
	}
	if len(parts) == 0 {
		h.fail(c, fmt.Errorf("%w: no file part supplied", upload.ErrInvalidArchive))
		return
	}

	only := parts[0]
	if len(parts) == 1 && bytes.HasPrefix(only.data, zipMagic) {
		if !h.withinQuota(c, to, int64(len(only.data))) {
			return
		}
		opts := upload.Options{
			Limits:           h.cfg.Upload.Limits(),
			FlattenRoot:      c.Query("flatten") == "true",
			RequireIndexHTML: true,
		}
		h.storeUpload(c, to, func(p string) (upload.Result, error) {
			opts.Prefix = p
			return upload.FromZip(ctx, h.store, bytes.NewReader(only.data), opts)
		})
		return
	}

	var total int64
	files := make([]upload.IncomingFile, 0, len(parts))
	for _, p := range parts {
		files = append(files, upload.IncomingFile{Name: p.name, Data: p.data, Size: int64(len(p.data))})
		total += int64(len(p.data))
	}
	if !h.withinQuota(c, to, total) {
		return
	}
	h.storeUpload(c, to, func(p string) (upload.Result, error) {
		return upload.FromFiles(ctx, h.store, files, upload.Options{
			Limits:           h.cfg.Upload.Limits(),
			RequireIndexHTML: true,
			Prefix:           p,
		})
	})
}

// ownedSite resolves the :id parameter against the caller's account, answering
// the request itself when the site is not theirs.
func (h *Handler) ownedSite(c *gin.Context) (*database.Site, bool) {
	a := currentAccount(c)

	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "That site id is not a number."})
		return nil, false
	}

	site, err := h.db.GetOwnedSite(c.Request.Context(), uint(id), a.User.ID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			// Not found rather than forbidden, so asking cannot confirm that
			// somebody else's site id exists.
			c.JSON(http.StatusNotFound, gin.H{"error": "No such site."})
			return nil, false
		}
		h.fail(c, err)
		return nil, false
	}
	return site, true
}

// deleteStaleKeys removes objects a redeploy no longer serves.
//
// A redeploy writes over the site's prefix, so a file the new manifest drops is
// still in the bucket, still unreachable by any URL the site uses, and still
// counted against the owner's quota. Comparing against the recorded manifest is
// enough to find them; nothing has to be listed, because the manifest is what
// the previous deployment wrote.
//
// A failure here is reported rather than swallowed: the site is live and
// correct either way, but the quota would over-report until the next redeploy.
func (h *Handler) deleteStaleKeys(ctx context.Context, siteID uint, current []upload.File) error {
	previous, err := h.db.ManifestForSite(ctx, siteID)
	if err != nil {
		return err
	}
	if len(previous) == 0 {
		return nil
	}

	kept := make(map[string]struct{}, len(current))
	for _, f := range current {
		kept[f.Key] = struct{}{}
	}

	for _, key := range previous {
		if _, still := kept[key]; still {
			continue
		}
		if err := h.store.DeleteKey(ctx, key); err != nil {
			return fmt.Errorf("delete stale key %s: %w", key, err)
		}
	}
	return nil
}

type ownerContext struct {
	ctx     context.Context
	ownerID string
	siteID  uint
}
