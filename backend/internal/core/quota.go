package core

import (
	"context"
	"fmt"
)

// CheckQuota refuses an upload that would take the account past its allowance.
//
// It runs before any bytes are written, because writing first and checking
// after would mean storing objects that are already over the limit. A redeploy
// that replaces the site's current deployment is credited back the bytes that
// deployment holds, since those are about to stop existing.
//
// An anonymous upload is not metered and never refuses.
func (c *Core) CheckQuota(ctx context.Context, to Target, incoming int64) error {
	if !to.Owned() {
		return nil
	}

	limit := c.cfg.Quota.BytesPerUser
	if limit <= 0 {
		return nil
	}

	used, err := c.db.UsageBytes(ctx, to.OwnerID)
	if err != nil {
		return err
	}

	credit := int64(0)
	if to.SiteID != 0 {
		previous, err := c.db.UsageBySite(ctx, to.SiteID)
		if err != nil {
			return err
		}
		credit = previous
	}

	if used-credit+incoming > limit {
		return fmt.Errorf("%w: this upload would use %d of %d bytes",
			ErrQuota, used-credit+incoming, limit)
	}
	return nil
}
