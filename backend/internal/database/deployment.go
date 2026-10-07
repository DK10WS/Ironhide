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

	token, err := NewClaimToken()
	if err != nil {
		return nil, fmt.Errorf("generate claim token: %w", err)
	}

	now := time.Now().UTC()
	dep := &Deployment{
		SiteID:         in.SiteID,
		Prefix:         prefix,
		Status:         StatusLive,
		FileCount:      len(in.Files),
		Bytes:          manifestTotal(in.Files),
		Manifest:       Manifest(in.Files),
		ClaimTokenHash: HashClaimToken(token),
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// An owned deployment is not a drop: it has no claim window, because there
	// is nothing to claim. Leaving ExpiresAt zero makes Expired() false
	// forever, which is what the sweeper checks.
	if in.ClaimedBy != "" {
		dep.ClaimedAt = &now
		dep.ClaimedBy = in.ClaimedBy
		dep.ClaimTokenHash = ""
	} else if in.TTL > 0 {
		dep.ExpiresAt = now.Add(in.TTL)
	}

	if err := d.db.WithContext(ctx).Create(dep).Error; err != nil {
		return nil, fmt.Errorf("insert deployment: %w", err)
	}

	return &CreateDeploymentResult{Deployment: dep, ClaimToken: token}, nil
}

// ClaimDeployment redeems a claim token, attaching the drop to a site.
//
// The UPDATE is conditional on claimed_at still being NULL, which is what makes
// the token single-use under concurrency: two simultaneous requests produce one
// winner and one ErrAlreadyClaimed, decided by the row lock rather than by a
// read-then-write race.
func (d *Database) ClaimDeployment(ctx context.Context, token string, siteID uint, claimedBy string) (*Deployment, error) {
	hash := HashClaimToken(token)
	if hash == "" {
		return nil, ErrNotFound
	}

	now := time.Now().UTC()

	res := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("claim_token_hash = ? AND claimed_at IS NULL", hash).
		// A drop whose window closed cannot be claimed even if the token is
		// still valid and the row was not swept yet.
		Where("expires_at > ?", now).
		Updates(map[string]any{
			"site_id":    siteID,
			"claimed_at": &now,
			"claimed_by": claimedBy,
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

	return d.GetDeploymentBySite(ctx, siteID)
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
	err := d.db.WithContext(ctx).Where("prefix = ?", p).First(&dep).Error
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
		Where("claimed_at IS NULL AND expires_at <= ?", time.Now().UTC()).
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
