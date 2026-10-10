package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/dk10ws/ironhide/internal/upload"
)

// Errors returned by this file. Callers map these onto transport responses.
var (
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("not found")

	// ErrAlreadyClaimed means the claim token was already used. This is
	// deliberately distinct from ErrNotFound so a caller cannot use the error
	// to probe whether a token exists.
	ErrAlreadyClaimed = errors.New("already claimed")

	// ErrInvalidPrefix means a prefix was empty or malformed.
	ErrInvalidPrefix = errors.New("invalid prefix")

	// ErrInvalidTTL means the claim window was negative.
	ErrInvalidTTL = errors.New("invalid ttl")

	// ErrInvalidTransition means a state change was requested from a state
	// the deployment is not in, which means something else already moved it.
	ErrInvalidTransition = errors.New("invalid state transition")
)

// CreateDeploymentInput is everything needed to record one upload.
//
// It is plain data on purpose: this package does not import the upload
// package, so persistence stays independent of how the bytes arrived. The
// caller maps an upload.Result onto these fields.
type CreateDeploymentInput struct {
	// SiteID attaches the deployment to a site. Nil creates an anonymous drop,
	// which is the Cloudflare-style temporary deployment that is only
	// reachable through its claim token.
	SiteID *uint

	// Prefix is the Garage key prefix, with or without a trailing slash.
	// Normalised before storage so PublicURLPath and the object keys agree.
	Prefix string

	// Files is the manifest of stored objects.
	Files []ManifestEntry

	// TTL is how long the drop stays claimable. Zero means no expiry, which
	// is only appropriate for a deployment already attached to a site.
	TTL time.Duration

	// ClaimedBy is set when the deployment is created already owned, for
	// example a deploy through the CLI rather than an anonymous drop.
	ClaimedBy string

	// OwnerID scopes the drop to an account. Empty means anonymous.
	OwnerID string
}

// CreateDeploymentResult carries the row plus the one secret that cannot be
// recovered from the database.
type CreateDeploymentResult struct {
	Deployment *Deployment

	// ClaimToken is the plaintext single-use token, returned exactly once.
	// Only its SHA-256 is stored. If the caller loses it, the drop cannot be
	// claimed and must expire.
	ClaimToken string
}

// CreateDeployment inserts the row for a completed upload.
//
// It fills every column the deployment needs to be servable and expirable:
// prefix, manifest, file count, byte total, status, expiry and the claim token
// hash.
//
// On consistency with Garage: this is a single-row insert, so it is atomic on
// its own, but it cannot be part of a transaction with the S3 writes. The
// caller owns the ordering. Uploading first and inserting second leaves
// orphaned objects if the insert fails; inserting first leaves an orphan row if
// the upload fails. Either way a reconciler or sweeper is needed, and this
// function does not attempt to hide that.
func (d *Database) CreateDeployment(ctx context.Context, in CreateDeploymentInput) (*CreateDeploymentResult, error) {
	prefix := NormalizePrefix(in.Prefix)
	if prefix == "" {
		return nil, ErrInvalidPrefix
	}

	// A negative TTL is rejected rather than absorbed. It previously fell
	// through the "TTL > 0" branch below, leaving ExpiresAt zero, and a zero
	// ExpiresAt reads as never expiring — so a caller passing -1 got a
	// permanent deployment instead of an error. That is the worst possible
	// failure for the sweeper: a site nothing will ever clean up, created by
	// what looks like a harmless mistake.
	//
	// Zero is still meaningful and means "no expiry", which is what a
	// deployment attached to a site wants.
	if in.TTL < 0 {
		return nil, fmt.Errorf("%w: must not be negative, got %s", ErrInvalidTTL, in.TTL)
	}

	token, err := NewClaimToken()
	if err != nil {
		return nil, fmt.Errorf("generate claim token: %w", err)
	}

	now := time.Now().UTC()

	// An owned deployment has no upload phase: its objects already exist and
	// there is nothing to claim, so it is born live. Creating it pending would
	// leave it looking like an upload that never finished, which is exactly
	// what the reconciler exists to abandon.
	status := StatusPending
	if in.ClaimedBy != "" {
		status = StatusLive
	}

	dep := &Deployment{
		SiteID:         in.SiteID,
		Prefix:         prefix,
		Status:         status,
		FileCount:      len(in.Files),
		Bytes:          manifestTotal(in.Files),
		Manifest:       Manifest(in.Files),
		ClaimTokenHash: HashClaimToken(token),
		OwnerID:        in.OwnerID,
		ClaimedBy:      in.ClaimedBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// An owned deployment is not a drop: it has no claim window, because there
	// is nothing to claim. A nil ExpiresAt is stored as NULL, which no
	// expiry comparison can match.
	if in.ClaimedBy != "" {
		dep.ClaimedAt = &now
		dep.ClaimedBy = in.ClaimedBy
		dep.ClaimTokenHash = ""
	} else if in.TTL > 0 {
		expires := now.Add(in.TTL)
		dep.ExpiresAt = &expires
	}

	if err := d.db.WithContext(ctx).Create(dep).Error; err != nil {
		return nil, fmt.Errorf("insert deployment: %w", err)
	}

	return &CreateDeploymentResult{Deployment: dep, ClaimToken: token}, nil
}

// RecordUpload attaches the manifest to a pending deployment and promotes it
// to live in one statement.
//
// Doing both together means a deployment is never live with an empty
// manifest, which would make the serve path unable to find its index.
func (d *Database) RecordUpload(ctx context.Context, id uint, res upload.Result) error {
	entries := make([]ManifestEntry, 0, len(res.Files))
	for _, f := range res.Files {
		entries = append(entries, ManifestEntry{Key: f.Key, Size: f.Size, ETag: f.ETag})
	}

	res1 := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("id = ? AND status = ?", id, StatusPending).
		Updates(map[string]any{
			"status":     StatusLive,
			"manifest":   Manifest(entries),
			"file_count": len(entries),
			"bytes":      res.Bytes,
			"updated_at": time.Now().UTC(),
		})
	if res1.Error != nil {
		return fmt.Errorf("record upload: %w", res1.Error)
	}
	if res1.RowsAffected == 0 {
		return fmt.Errorf("%w: deployment %d is not pending", ErrInvalidTransition, id)
	}
	return nil
}

// MarkLive promotes a pending deployment to live.
//
// It is conditional on the row still being pending, so a reconciler that
// already failed a stuck upload cannot have it promoted afterwards.
func (d *Database) MarkLive(ctx context.Context, id uint) error {
	return d.transition(ctx, id, StatusPending, StatusLive, nil)
}

// MarkFailed abandons a deployment whose objects never arrived, or that a
// reconciler found stuck.
//
// The row is kept rather than deleted so the prefix stays visible for
// diagnosis, and so the same prefix cannot be re-registered by a later
// request.
func (d *Database) MarkFailed(ctx context.Context, id uint) error {
	now := time.Now().UTC()
	return d.transition(ctx, id, StatusPending, StatusFailed, map[string]any{"updated_at": now})
}

// transition moves a deployment between states, only from the expected one.
func (d *Database) transition(ctx context.Context, id uint, from, to string, extra map[string]any) error {
	cols := map[string]any{"status": to}
	for k, v := range extra {
		cols[k] = v
	}

	res := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("id = ? AND status = ?", id, from).
		Updates(cols)
	if res.Error != nil {
		return fmt.Errorf("transition to %s: %w", to, res.Error)
	}
	if res.RowsAffected == 0 {
		// Already moved on, or gone. Not an error: a reconciler and a handler
		// racing over the same row is normal, and the outcome is correct
		// either way.
		return nil
	}
	return nil
}

// DeleteDeployment removes a row outright.
//
// It is used when an upload is rejected before writing anything: the row's
// purpose is to describe objects, and with no objects there is nothing to
// describe. Keeping a failed row for every bad request would let an
// unauthenticated caller fill the table for free.
func (d *Database) DeleteDeployment(ctx context.Context, id uint) error {
	if err := d.db.WithContext(ctx).Delete(&Deployment{}, id).Error; err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	return nil
}

// MarkStalePendingFailed abandons pending deployments older than grace.
//
// This is the reconciler. It works on rows rather than on objects precisely
// because a stuck upload leaves a row behind: scanning the bucket for objects
// with no row would mean listing every key, whereas this is one indexed query.
//
// grace must exceed the longest plausible upload, or a slow one gets abandoned
// while it is still being written.
func (d *Database) MarkStalePendingFailed(ctx context.Context, grace time.Duration, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	cutoff := time.Now().UTC().Add(-grace)

	// Claim the batch before changing it. SKIP LOCKED keeps two reconcilers
	// from doing the same work: the second one takes the rows the first has
	// locked rather than blocking behind them.
	var stale []Deployment
	err := d.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("status = ? AND created_at < ?", StatusPending, cutoff).
		// claimed_at IS NULL is belt and braces alongside the status check.
		// Anything somebody owns is off limits to this query whatever else is
		// true about the row, so no future change to the status handling can
		// make it delete a real site.
		Where("claimed_at IS NULL").
		Order("created_at ASC").
		Limit(limit).
		Find(&stale).Error
	if err != nil {
		return nil, fmt.Errorf("find stale pending: %w", err)
	}
	if len(stale) == 0 {
		return nil, nil
	}

	ids := make([]uint, 0, len(stale))
	for i := range stale {
		ids = append(ids, stale[i].ID)
	}

	res := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("id IN ? AND status = ? AND claimed_at IS NULL", ids, StatusPending).
		Updates(map[string]any{"status": StatusFailed, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return nil, fmt.Errorf("mark stale pending failed: %w", res.Error)
	}

	return stale, nil
}

// ListStaleFailed returns failed deployments old enough to be destroyed.
//
// The second stage of removal. A row reaches it only after the reconciler has
// already decided the upload did not complete, and only once it has sat there
// past grace, which is what separates "stopped trusting this" from "destroyed
// it".
func (d *Database) ListStaleFailed(ctx context.Context, grace time.Duration, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	cutoff := time.Now().UTC().Add(-grace)

	var out []Deployment
	err := d.db.WithContext(ctx).
		Where("status = ? AND claimed_at IS NULL AND updated_at < ?", StatusFailed, cutoff).
		Order("updated_at ASC").
		Limit(limit).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list stale failed: %w", err)
	}
	return out, nil
}

// ClaimForPurge marks a deployment as being purged and reports whether this
// caller won.
//
// from is the status the row must still be in. Callers differ because they
// arrive at purging from different states: the purger from failed, the
// sweeper from an expired live drop. Making it explicit stops a sweeper from
// claiming a row the purger has already handled, or either from claiming a
// pending upload.
//
// The state moves to a distinct value rather than being deleted straight away,
// so the row stays visible and the delete can be retried if it fails partway.
func (d *Database) ClaimForPurge(ctx context.Context, id uint, from string) (*Deployment, bool, error) {
	var dep Deployment

	res := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Clauses(clause.Returning{}).
		Where("id = ? AND status = ? AND claimed_at IS NULL", id, from).
		Updates(map[string]any{"status": StatusPurging, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return nil, false, fmt.Errorf("claim for purge: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, false, nil
	}

	err := d.db.WithContext(ctx).First(&dep, id).Error
	if err != nil {
		return nil, false, fmt.Errorf("load claimed deployment: %w", err)
	}
	return &dep, true, nil
}

// ListExpiredForSweep returns unclaimed drops whose claim window has closed.
//
// This is the expiry path's work list. Three exclusions matter:
//
//   - claimed_at IS NULL: anything somebody owns is permanent and must never
//     appear here, whatever its status says.
//   - expires_at IS NOT NULL: "no expiry" is stored as NULL, and a comparison
//     would never match it anyway. Spelled out so the intent survives a
//     rewrite of this query.
//   - status is live or already purging. Pending is excluded deliberately:
//     CreateDeployment sets an expiry before the upload starts, so keying on
//     expiry alone would let a slow upload be destroyed mid-write. Purging is
//     included so a sweep interrupted partway retries instead of stranding a
//     half-deleted site.
func (d *Database) ListExpiredForSweep(ctx context.Context, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var out []Deployment
	err := d.db.WithContext(ctx).
		Where("claimed_at IS NULL").
		Where("expires_at IS NOT NULL AND expires_at <= ?", time.Now().UTC()).
		Where("status IN ?", []string{StatusLive, StatusPurging}).
		Order("expires_at ASC").
		Limit(limit).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list expired for sweep: %w", err)
	}
	return out, nil
}

// ClaimPermanently redeems a claim token to stop a drop from expiring.
//
// The token is the credential: there is no other authentication on this path,
// which is why it is single-use, 256 bits wide, and never stored in plaintext.
//
// The UPDATE is conditional on claimed_at still being NULL, which is what makes
// the token single-use under concurrency: two simultaneous requests produce one
// winner and one ErrAlreadyClaimed, decided by the row lock rather than by a
// read-then-write race.
//
// Clearing expires_at is the whole point. It is set to NULL rather than a far
// future date so the row becomes indistinguishable from one that was never a
// drop, and no sweeper query can select it.
func (d *Database) ClaimPermanently(ctx context.Context, token, claimedBy string) (*Deployment, error) {
	hash := HashClaimToken(token)
	if hash == "" {
		return nil, ErrNotFound
	}

	now := time.Now().UTC()
	dep := &Deployment{}

	res := d.db.WithContext(ctx).
		Model(dep).
		Clauses(clause.Returning{}).
		Where("claim_token_hash = ? AND claimed_at IS NULL", hash).
		// Only a live drop can be claimed. A pending one is still being
		// written, and its objects may not all exist yet.
		Where("status = ?", StatusLive).
		// A drop whose window closed cannot be claimed even if the token is
		// still valid and the row has not been swept yet.
		Where("expires_at IS NOT NULL AND expires_at > ?", now).
		Updates(map[string]any{
			"claimed_at": &now,
			"claimed_by": claimedBy,
			"expires_at": nil,
			"status":     StatusLive,
			"updated_at": now,
			// The token is spent. Clearing the hash also means a database dump
			// taken later cannot be used to claim anything still outstanding.
			"claim_token_hash": "",
		})

	if res.Error != nil {
		return nil, fmt.Errorf("claim deployment: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, ErrAlreadyClaimed
	}

	// RETURNING filled dep from the row it just updated, so this does not need
	// a second query keyed on a hash that no longer exists.
	if dep.ID == 0 {
		return nil, ErrNotFound
	}
	return dep, nil
}

// GetDeployment loads one deployment by its primary key.
func (d *Database) GetDeployment(ctx context.Context, id uint) (*Deployment, error) {
	var dep Deployment
	err := d.db.WithContext(ctx).First(&dep, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	return &dep, nil
}

// GetDeploymentByPrefix loads a deployment by its object prefix.
func (d *Database) GetDeploymentByPrefix(ctx context.Context, prefix string) (*Deployment, error) {
	p := NormalizePrefix(prefix)
	if p == "" {
		return nil, ErrInvalidPrefix
	}

	var dep Deployment
	// A redeploy writes the new deployment over the old one's prefix, so a
	// prefix can carry several rows and only one of them is live. Asking for
	// the live one first is what stops the retired row being served, which 410s
	// a site whose replacement is sitting right there holding the manifest.
	err := d.db.WithContext(ctx).
		Where("prefix = ? AND status = ?", p, StatusLive).
		Order("id DESC").
		First(&dep).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Nothing live under this prefix. Fall back to the newest row of any
		// status, so an expired or abandoned prefix still comes back as a row
		// and the serve path can answer with the reason rather than a bare 404
		// that reads as a mistyped address.
		err = d.db.WithContext(ctx).
			Where("prefix = ?", p).
			Order("id DESC").
			First(&dep).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment by prefix: %w", err)
	}
	return &dep, nil
}

// GetDeploymentBySite returns the live deployment for a site, if any.
//
// This is the read path for serving a claimed site: the newest live row wins,
// so a rollback is just an insert or a status change.
func (d *Database) GetDeploymentBySite(ctx context.Context, siteID uint) (*Deployment, error) {
	var dep Deployment
	err := d.db.WithContext(ctx).
		Where("site_id = ? AND status = ?", siteID, StatusLive).
		Order("id DESC").
		First(&dep).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment for site: %w", err)
	}
	return &dep, nil
}

// ListExpired returns unclaimed drops whose window has closed, oldest first.
//
// This is the sweeper's work list. It deliberately excludes anything already
// claimed: a claimed deployment is permanent and must never be swept.
func (d *Database) ListExpired(ctx context.Context, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var out []Deployment
	err := d.db.WithContext(ctx).
		// expires_at IS NOT NULL is explicit even though the comparison
		// already excludes NULL, so the intent survives a future rewrite.
		Where("claimed_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?", time.Now().UTC()).
		Order("expires_at ASC").
		Limit(limit).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list expired: %w", err)
	}
	return out, nil
}

// MarkExpired flips an expired drop out of the live set.
//
// It is conditional on the row still being unclaimed, so a drop claimed between
// ListExpired and this call is left alone. Returns the number of rows changed
// so the caller knows whether it still needs to delete the objects.
func (d *Database) MarkExpired(ctx context.Context, id uint) (int64, error) {
	res := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("id = ? AND claimed_at IS NULL", id).
		Updates(map[string]any{
			"status":           StatusExpired,
			"claim_token_hash": "",
			"updated_at":       time.Now().UTC(),
		})
	if res.Error != nil {
		return 0, fmt.Errorf("mark expired: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// ListDeploymentsForSite returns a site's deployment history, newest first.
func (d *Database) ListDeploymentsForSite(ctx context.Context, siteID uint, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var out []Deployment
	err := d.db.WithContext(ctx).
		Preload("Site").
		Where("site_id = ?", siteID).
		Order("id DESC").
		Limit(limit).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	return out, nil
}

// UpsertSite creates or updates the record for a hostname.
func (d *Database) UpsertSite(ctx context.Context, slug, domain, ownerID string) (*Site, error) {
	if slug == "" || domain == "" {
		return nil, errors.New("slug and domain are required")
	}

	site := Site{Slug: slug, Domain: domain, OwnerID: ownerID}

	// DoUpdate covers the re-attach case: claiming a drop for a domain that
	// already has a site row reuses that row rather than colliding with the
	// unique index on domain.
	err := d.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "domain"}},
			DoUpdates: clause.AssignmentColumns([]string{"owner_id", "updated_at"}),
		}).
		Create(&site).Error
	if err != nil {
		return nil, fmt.Errorf("upsert site: %w", err)
	}
	return &site, nil
}

// GetSiteByDomain resolves a hostname to its site.
func (d *Database) GetSiteByDomain(ctx context.Context, domain string) (*Site, error) {
	var site Site
	err := d.db.WithContext(ctx).Where("domain = ?", domain).First(&site).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get site by domain: %w", err)
	}
	return &site, nil
}

// claimTokenBytes is 32 bytes: 256 bits, so the token cannot be guessed or
// brute-forced within any plausible number of attempts.
const claimTokenBytes = 32

// NewClaimToken returns a fresh single-use claim token.
//
// base64 RawURL is used so the token can be dropped into a URL path or query
// without escaping.
func NewClaimToken() (string, error) {
	var b [claimTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashClaimToken returns the hex SHA-256 stored in the database.
//
// A hash rather than the token itself, so a database dump does not hand over
// every outstanding drop. SHA-256 is safe here because the input is 256 bits
// of CSPRNG output, so there is nothing to brute-force.
func HashClaimToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NormalizePrefix strips leading and trailing slashes.
//
// The upload package generates prefixes with a trailing slash because they are
// used to build keys directly, while the database wants a clean identifier.
// Normalising in one place stops PublicURLPath from producing a doubled slash.
func NormalizePrefix(prefix string) string {
	return strings.Trim(strings.TrimSpace(prefix), "/")
}

func manifestTotal(files []ManifestEntry) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}
