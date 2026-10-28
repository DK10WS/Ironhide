package core

import (
	"fmt"
	"strings"
)

// PreviewURL builds the absolute address a deployment is served from.
func (c *Core) PreviewURL(slug string) string {
	return fmt.Sprintf("%s://%s%s/", c.cfg.DataScheme, slug, c.cfg.DataHostSuffix)
}

// ClaimURL builds the single-use claim link for a drop.
func (c *Core) ClaimURL(token string) string {
	return c.cfg.PublicURL + "/claim/" + token
}

// DataHost is the host part of a preview URL, which is what a site row stores.
func (c *Core) DataHost(slug string) string {
	return strings.TrimPrefix(c.PreviewURL(slug), c.cfg.DataScheme+"://")
}

// ArchiveLimit is the largest archive an upload may carry.
//
// A client that has to build an archive before sending it can use this to stop
// before it has buffered something the upload will refuse anyway.
func (c *Core) ArchiveLimit() int64 {
	return c.cfg.Upload.ArchiveBytes
}
