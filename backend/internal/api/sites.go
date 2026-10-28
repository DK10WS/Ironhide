package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/core"
	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

// registerSites wires the account's own resources. Every route here requires a
// session, so none of them can be reached anonymously.
func (h *Handler) registerSites(r *gin.RouterGroup) {
	my := r.Group("/my", h.requireAccount)
	{
		my.GET("/sites", h.mySites)
		my.DELETE("/sites/:id", h.deleteSite)
		my.POST("/sites/:id/deploy", h.redeployToSite)
		my.POST("/sites/:id/pause", h.pauseSite)
		my.POST("/sites/:id/resume", h.resumeSite)
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
	Paused     bool   `json:"paused"`
}

// mySites lists the signed-in account's sites.
func (h *Handler) mySites(c *gin.Context) {
	a := currentAccount(c)

	sites, err := h.core.ListSites(c.Request.Context(), a.User.ID)
	if err != nil {
		h.fail(c, err)
		return
	}

	out := make([]siteView, 0, len(sites))
	for _, s := range sites {
		view := siteView{
			ID:     s.ID,
			Slug:   s.Slug,
			URL:    s.URL,
			Bytes:  s.Bytes,
			Files:  s.Files,
			Live:   s.Live,
			Paused: s.Paused,
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

	u, err := h.core.Usage(c.Request.Context(), a.User.ID)
	if err != nil {
		h.fail(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"used":  u.Used,
		"limit": u.Limit,
	})
}

// deleteSite removes a site from its owner's account.
func (h *Handler) deleteSite(c *gin.Context) {
	a := currentAccount(c)

	id, ok := siteID(c)
	if !ok {
		return
	}

	if err := h.core.DeleteSite(c.Request.Context(), a.User.ID, id); err != nil {
		h.siteFail(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "id": id})
}

// pauseSite takes a site down. The site keeps its address and its objects, so
// bringing it back is immediate rather than a redeploy.
func (h *Handler) pauseSite(c *gin.Context) {
	h.setPaused(c, true)
}

// resumeSite brings a site back up.
func (h *Handler) resumeSite(c *gin.Context) {
	h.setPaused(c, false)
}

func (h *Handler) setPaused(c *gin.Context, paused bool) {
	a := currentAccount(c)

	id, ok := siteID(c)
	if !ok {
		return
	}

	if err := h.core.SetPaused(c.Request.Context(), a.User.ID, id, paused); err != nil {
		h.siteFail(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "id": id, "paused": paused})
}

// targetFor works out who an ordinary upload is for.
//
// A session turns the upload into an owned, permanent site with no claim link.
// Without one it stays a temporary drop, which is the behaviour that existed
// before accounts did and is still the only option for an anonymous caller.
func (h *Handler) targetFor(c *gin.Context) core.Target {
	value, ok := c.Get(accountKey)
	if !ok {
		return core.Target{}
	}
	a := value.(account)
	if a.User == nil {
		return core.Target{}
	}
	return core.Target{OwnerID: a.User.ID}
}

// redeployToSite replaces the contents of a site the caller owns.
//
// One route covers both shapes: a single part that starts with a zip header is
// an archive, anything else is the site itself. That keeps the dashboard's
// redeploy to one button instead of three.
func (h *Handler) redeployToSite(c *gin.Context) {
	a := currentAccount(c)

	id, ok := siteID(c)
	if !ok {
		return
	}

	parts, err := readParts(c, h.cfg.Upload.MaxBytes)
	if err != nil {
		h.fail(c, err)
		return
	}
	if len(parts) == 0 {
		h.fail(c, fmt.Errorf("%w: no file part supplied", upload.ErrInvalidArchive))
		return
	}

	payload := core.Payload{Flatten: c.Query("flatten") == "true"}
	if len(parts) == 1 && core.IsArchive(parts[0].data) {
		payload.Zip = parts[0].data
	} else {
		payload.Files = incomingFiles(parts)
	}

	res, err := h.core.Redeploy(c.Request.Context(), a.User.ID, id, payload)
	if err != nil {
		h.siteFail(c, err)
		return
	}

	c.JSON(http.StatusCreated, uploadBody(res))
}

// siteID parses the :id parameter, answering the request itself when it is not
// a number.
func siteID(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "That site id is not a number."})
		return 0, false
	}
	return uint(id), true
}

// siteFail answers a site operation that could not be scoped to the caller.
//
// Not found rather than forbidden, so asking cannot confirm that somebody
// else's site id exists.
func (h *Handler) siteFail(c *gin.Context, err error) {
	if errors.Is(err, database.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "No such site."})
		return
	}
	h.fail(c, err)
}
