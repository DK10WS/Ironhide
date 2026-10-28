package core

import "context"

// ListSites returns an account's sites, newest deployment first.
func (c *Core) ListSites(ctx context.Context, ownerID string) ([]Site, error) {
	rows, err := c.db.ListOwnedSites(ctx, ownerID)
	if err != nil {
		return nil, err
	}

	out := make([]Site, 0, len(rows))
	for _, s := range rows {
		out = append(out, Site{
			ID:         s.ID,
			Slug:       s.Slug,
			URL:        c.PreviewURL(s.Slug),
			Bytes:      s.Bytes,
			Files:      s.Files,
			DeployedAt: s.DeployedAt,
			Live:       s.Live,
			Paused:     s.Paused,
		})
	}
	return out, nil
}

// DeleteSite removes a site from its owner's account.
//
// The objects are not deleted here. Garage has no single-call prefix delete and
// a large site can take many batched round trips, which would hold the caller
// open far too long. Instead the site row goes and its deployments become
// anonymous drops, which the purger already knows how to destroy. The account
// stops being charged for them at the same moment, because owner_id is cleared
// in the same transaction.
func (c *Core) DeleteSite(ctx context.Context, ownerID string, siteID uint) error {
	site, err := c.db.GetOwnedSite(ctx, siteID, ownerID)
	if err != nil {
		return err
	}
	return c.db.ReleaseSite(ctx, site.ID)
}

// SetPaused takes a site down or brings it back up.
//
// The site keeps its address and its objects, so bringing it back is immediate
// rather than a redeploy.
func (c *Core) SetPaused(ctx context.Context, ownerID string, siteID uint, paused bool) error {
	site, err := c.db.GetOwnedSite(ctx, siteID, ownerID)
	if err != nil {
		return err
	}
	return c.db.SetSitePaused(ctx, site.ID, ownerID, paused)
}

// Usage reports how much of the allowance is spent, so a client can draw the
// bar without recomputing it.
func (c *Core) Usage(ctx context.Context, ownerID string) (Usage, error) {
	used, err := c.db.UsageBytes(ctx, ownerID)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Used: used, Limit: c.cfg.Quota.BytesPerUser}, nil
}
