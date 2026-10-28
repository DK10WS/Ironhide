package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

// zipMagic is the local file header every zip archive starts with.
//
// Detection is by content, never by filename or a client-supplied type,
// because both are attacker-controlled: a caller can name a bomb "index.html".
var zipMagic = []byte{'P', 'K', 0x03, 0x04}

// IsArchive reports whether a body starts with a zip header.
//
// It is exported because a caller that has to choose between the archive route
// and the file route needs the same test, and two copies of it would be free to
// disagree.
func IsArchive(body []byte) bool {
	return bytes.HasPrefix(body, zipMagic)
}

// Upload stores a site or a drop.
func (c *Core) Upload(ctx context.Context, to Target, p Payload) (Result, error) {
	if p.Zip != nil {
		if len(p.Zip) > int(c.cfg.Upload.ArchiveBytes) {
			return Result{}, fmt.Errorf("%w: archive exceeds %d bytes",
				upload.ErrTooLarge, c.cfg.Upload.ArchiveBytes)
		}
		if !IsArchive(p.Zip) {
			return Result{}, fmt.Errorf("%w: body does not start with a zip header",
				upload.ErrInvalidArchive)
		}
		// The archive is compressed, so this bounds the request rather than the
		// result; the real total is re-checked against the quota once the
		// manifest is known.
		if err := c.CheckQuota(ctx, to, int64(len(p.Zip))); err != nil {
			return Result{}, err
		}

		return c.write(ctx, to, func(prefix string) (upload.Result, error) {
			return upload.FromZip(ctx, c.store, bytes.NewReader(p.Zip), upload.Options{
				Limits:           c.cfg.Upload.Limits(),
				FlattenRoot:      p.Flatten,
				RequireIndexHTML: true,
				AllowSinglePDF:   true,
				Prefix:           prefix,
			})
		})
	}

	if len(p.Files) == 0 {
		return Result{}, fmt.Errorf("%w: no files supplied", upload.ErrInvalidArchive)
	}

	// Uncompressed, so this is the figure the quota is actually measured in.
	var total int64
	for _, f := range p.Files {
		total += f.Size
	}
	if err := c.CheckQuota(ctx, to, total); err != nil {
		return Result{}, err
	}

	return c.write(ctx, to, func(prefix string) (upload.Result, error) {
		return upload.FromFiles(ctx, c.store, p.Files, upload.Options{
			Limits:           c.cfg.Upload.Limits(),
			RequireIndexHTML: true,
			AllowSinglePDF:   true,
			Prefix:           prefix,
		})
	})
}

// Redeploy replaces a site's contents under its existing address.
//
// The objects go under the site's current prefix, so the URL does not change;
// the deployment row is retired and a new one recorded, so the manifest and the
// bucket cannot drift apart.
func (c *Core) Redeploy(ctx context.Context, ownerID string, siteID uint, p Payload) (Result, error) {
	site, err := c.db.GetOwnedSite(ctx, siteID, ownerID)
	if err != nil {
		return Result{}, err
	}

	// The prefix of the site's current deployment is the site's address. With
	// no deployment yet the site's slug is the whole of it.
	prefix := "uploads/" + site.Slug
	if dep, err := c.db.GetDeploymentBySite(ctx, siteID); err == nil && dep != nil {
		prefix = dep.Prefix
	}
	// Stored prefixes are normalised, which strips the trailing separator, but
	// object keys are built by joining the prefix and the relative path. A
	// prefix that lost it writes every object as "uploads/<slug>index.html",
	// and the serve path then finds no manifest entry at the root at all.
	prefix = strings.TrimSuffix(prefix, "/") + "/"

	return c.Upload(ctx, Target{OwnerID: ownerID, SiteID: siteID, Prefix: prefix}, p)
}

// write runs the three-step write every upload shape shares.
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
func (c *Core) write(ctx context.Context, to Target, store func(prefix string) (upload.Result, error)) (Result, error) {
	// A redeploy passes its existing prefix. Otherwise a fresh one is minted
	// here rather than inside upload so the row can carry it. It is still
	// server-generated: nothing about it comes from the caller, which is what
	// keeps one upload from addressing another.
	prefix := to.Prefix
	if prefix == "" {
		prefix = upload.NewPrefix()
	}

	input := database.CreateDeploymentInput{Prefix: prefix}
	if to.Owned() {
		// An owned deployment is not a drop: it has no claim window, because
		// there is nothing to claim and nothing to expire.
		input.OwnerID = to.OwnerID
		input.TTL = 0
	} else {
		input.TTL = c.cfg.Upload.TTL
	}

	created, err := c.db.CreateDeployment(ctx, input)
	if err != nil {
		return Result{}, err
	}

	// No files are known yet; RecordUpload fills the counts in once the objects
	// are really there.
	res, err := store(prefix)
	if err != nil {
		c.abandon(ctx, created.Deployment.ID, res)
		return Result{}, err
	}

	if err := c.db.RecordUpload(ctx, created.Deployment.ID, res); err != nil {
		// The objects are stored but the manifest could not be written, so the
		// row cannot describe them. Fail it rather than serve a site whose
		// recorded contents disagree with the bucket.
		c.abandon(ctx, created.Deployment.ID, res)
		return Result{}, err
	}

	slug := created.Deployment.Slug()

	if to.Owned() {
		if err := c.attachOwned(ctx, to, created.Deployment.ID, slug, res); err != nil {
			// The objects exist but nothing ties them to the account, so the
			// row has to go rather than leave bytes nobody can find.
			c.abandon(ctx, created.Deployment.ID, res)
			return Result{}, err
		}
	}

	slog.Info("upload accepted",
		"id", created.Deployment.ID,
		"prefix", created.Deployment.Prefix,
		"files", len(res.Files),
		"bytes", res.Bytes,
		"owned", to.Owned(),
	)

	out := Result{
		ID:         created.Deployment.ID,
		Slug:       slug,
		PreviewURL: c.PreviewURL(slug),
		ExpiresAt:  created.Deployment.ExpiresAt,
		Bytes:      res.Bytes,
		Files:      res.Files,
		Owned:      to.Owned(),
	}
	if !to.Owned() {
		out.ClaimURL = c.ClaimURL(created.ClaimToken)
	}
	return out, nil
}

// attachOwned ties a finished upload to a site in the owner's account.
//
// A new site takes its name from the deployment's slug, which is the label the
// address is built from. A redeploy keeps the existing row so the address is
// unchanged, and retires the deployment it supersedes so the old bytes stop
// counting against the quota in the same transaction.
func (c *Core) attachOwned(ctx context.Context, to Target, deploymentID uint, slug string, res upload.Result) error {
	if to.SiteID != 0 {
		if err := c.db.AttachSiteToDeployment(ctx, deploymentID, to.SiteID, to.OwnerID); err != nil {
			return err
		}
		// A redeploy wrote over the old prefix, so anything the new manifest no
		// longer lists is now unreachable and still counted against the quota.
		return c.deleteStaleKeys(ctx, to.SiteID, res.Files)
	}

	site, err := c.db.UpsertSite(ctx, slug, c.DataHost(slug), to.OwnerID)
	if err != nil {
		return err
	}
	return c.db.AttachSiteToDeployment(ctx, deploymentID, site.ID, to.OwnerID)
}

// abandon deals with the row for an upload that did not finish.
//
// If nothing was written the row is deleted outright, because a row exists to
// describe objects and there are none. Keeping a failed row per rejected
// request would let an unauthenticated caller fill the deployments table for
// free, and almost every rejection is a validation failure that never touched
// the bucket.
//
// If objects did land, the row is marked failed instead: the reconciler will
// report the prefix so whatever was written can be found, and nothing is lost.
func (c *Core) abandon(ctx context.Context, id uint, res upload.Result) {
	var err error
	if len(res.Files) == 0 {
		err = c.db.DeleteDeployment(ctx, id)
	} else {
		err = c.db.MarkFailed(ctx, id)
	}
	if err != nil {
		slog.Error("could not clean up a failed upload",
			"id", id, "prefix", res.Prefix, "err", err)
	}
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
func (c *Core) deleteStaleKeys(ctx context.Context, siteID uint, current []upload.File) error {
	previous, err := c.db.ManifestForSite(ctx, siteID)
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
		if err := c.store.DeleteKey(ctx, key); err != nil {
			return fmt.Errorf("delete stale key %s: %w", key, err)
		}
	}
	return nil
}
