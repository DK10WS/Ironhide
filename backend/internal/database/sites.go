package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// OwnedSite is a site as its owner sees it: the address it is served from and
// what the current live deployment costs them.
type OwnedSite struct {
	Site

	// Live reports whether the site currently has a deployment behind it.
	Live bool

	// Bytes and Files describe that deployment. Zero when there is none.
	Bytes int64
	Files int

	// DeployedAt is when it went live, or nil if it never has.
	DeployedAt *time.Time

	// Paused is the site's up and down switch.
	Paused bool
}

// ListOwnedSites returns an account's sites, newest deployment first.
//
// The owner filter is in the query rather than applied afterwards, so a site
// belonging to someone else can never be read by asking for the wrong page.
func (d *Database) ListOwnedSites(ctx context.Context, ownerID string) ([]OwnedSite, error) {
	var rows []OwnedSite
	err := d.db.WithContext(ctx).
		Model(&Site{}).
		Select(`sites.*,
			sites.private AS paused,
			EXISTS (SELECT 1 FROM deployments d
			        WHERE d.site_id = sites.id AND d.status = ?) AS live,
			COALESCE((SELECT d2.bytes FROM deployments d2
			        WHERE d2.site_id = sites.id AND d2.status = ?
			        ORDER BY d2.id DESC LIMIT 1), 0) AS bytes,
			COALESCE((SELECT d3.file_count FROM deployments d3
			        WHERE d3.site_id = sites.id AND d3.status = ?
			        ORDER BY d3.id DESC LIMIT 1), 0) AS files,
			(SELECT MAX(d4.updated_at) FROM deployments d4
			        WHERE d4.site_id = sites.id AND d4.status = ?) AS deployed_at`, StatusLive, StatusLive, StatusLive, StatusLive).
		Where("owner_id = ?", ownerID).
		Order("sites.id DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list owned sites: %w", err)
	}
	return rows, nil
}

// GetOwnedSite returns one site, but only to the account that owns it.
//
// ErrNotFound covers both "no such site" and "not yours". Telling them apart
// would let a caller confirm that a site id exists, which is a small leak about
// someone else's account for no benefit.
func (d *Database) GetOwnedSite(ctx context.Context, id uint, ownerID string) (*Site, error) {
	if ownerID == "" {
		return nil, ErrNotFound
	}

	var site Site
	err := d.db.WithContext(ctx).
		Where("id = ? AND owner_id = ?", id, ownerID).
		First(&site).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get owned site: %w", err)
	}
	return &site, nil
}

// SetSitePaused takes a site down or brings it back up.
//
// The owner filter is in the update rather than applied afterwards, so a site
// belonging to someone else cannot be reached by naming its id. A site that is
// already in the requested state is not an error: the caller wanted it down,
// and it is down.
func (d *Database) SetSitePaused(ctx context.Context, id uint, ownerID string, paused bool) error {
	if ownerID == "" {
		return ErrNotFound
	}

	res := d.db.WithContext(ctx).
		Model(&Site{}).
		Where("id = ? AND owner_id = ?", id, ownerID).
		Update("private", paused)
	if res.Error != nil {
		return fmt.Errorf("set site paused: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// Either no such site, or it is already in the state asked for, in
		// which case there was nothing to change. The first is a refusal and
		// the second is a success, and they cannot be told apart from the
		// affected count, so the site's existence is checked separately.
		var site Site
		err := d.db.WithContext(ctx).
			Select("id").
			Where("id = ? AND owner_id = ?", id, ownerID).
			First(&site).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("check site: %w", err)
		}
	}
	return nil
}

// ReleaseSite detaches every deployment from a site and removes the site row.
//
// The deployments are not deleted here: their objects still exist and the row
// is what makes them findable. Clearing site_id and owner_id turns the site
// into an anonymous drop, which is the state the sweeper and purger already
// know how to clean up.
func (d *Database) ReleaseSite(ctx context.Context, id uint) error {
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Deployment{}).
			Where("site_id = ?", id).
			Updates(map[string]any{"site_id": nil, "owner_id": ""}).Error; err != nil {
			return fmt.Errorf("detach deployments: %w", err)
		}
		if err := tx.Delete(&Site{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("delete site: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// AttachSiteToDeployment points a deployment at a site it replaces.
//
// The previous live deployment for the site is retired in the same statement
// group, so an owner's quota stops counting the old bytes the moment the new
// ones are recorded. Doing it in one transaction means a redeploy cannot leave
// two live deployments, which would double-count against the quota.
func (d *Database) AttachSiteToDeployment(ctx context.Context, deploymentID, siteID uint, ownerID string) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Deployment{}).
			Where("site_id = ? AND id <> ? AND status = ?", siteID, deploymentID, StatusLive).
			Update("status", StatusExpired).Error; err != nil {
			return fmt.Errorf("retire previous deployment: %w", err)
		}
		err := tx.Model(&Deployment{}).
			Where("id = ?", deploymentID).
			Updates(map[string]any{
				"site_id":    siteID,
				"owner_id":   ownerID,
				"claimed_by": "account",
			}).Error
		if err != nil {
			return fmt.Errorf("attach deployment: %w", err)
		}
		return nil
	})
}

// ManifestForSite returns the object keys of a site's live deployment.
//
// Only the live row is consulted, because a retired deployment's objects may
// already have been deleted by a redeploy, and reporting them would make the
// caller try to delete keys that are gone.
func (d *Database) ManifestForSite(ctx context.Context, siteID uint) ([]string, error) {
	dep, err := d.GetDeploymentBySite(ctx, siteID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if dep == nil {
		return nil, nil
	}

	keys := make([]string, 0, len(dep.Manifest))
	for _, e := range dep.Manifest {
		keys = append(keys, e.Key)
	}
	return keys, nil
}
