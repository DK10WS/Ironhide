package database

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// Deployment states.
//
// A deployment is written as pending before its objects exist, promoted to
// live once they do, and abandoned as failed if the upload never completes.
//
// pending is what makes a failed upload findable: the row is created first,
// so bytes never exist without a row describing them. A reconciler ages stuck
// pending rows out to failed.
//
// Claiming does not change the state, only the owner, because a claimed drop
// is still live and still serves.
const (
	StatusPending = "pending"
	StatusFailed  = "failed"
	// StatusPurging is a transient state: the row is about to lose its objects
	// and then itself. It exists so a crash mid-delete leaves a findable
	// record rather than an invisible half-deletion.
	StatusPurging = "purging"
	StatusLive    = "live"
	StatusExpired = "expired"
	StatusDeleted = "deleted"
)

// User is an account, identified by Google's stable per-client subject id.
//
// The subject is the primary key rather than a surrogate: it is already unique,
// already stable across renames, and it is what Site.OwnerID and
// Deployment.OwnerID already hold. Using it avoids a mapping table whose only
// job would be to renumber a string.
type User struct {
	// ID is Google's "sub" claim for this client.
	ID string `gorm:"size:64;primaryKey"`

	Email string `gorm:"size:254;index"`

	// EmailVerified comes from Google. An unverified address is recorded but
	// never treated as a reason to trust anything beyond sign-in.
	EmailVerified bool `gorm:"not null;default:false"`

	Name    string `gorm:"size:128"`
	Picture string `gorm:"size:512"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Session is one signed-in browser.
//
// Only the token's hash is stored, so a copy of the table does not hand out
// working cookies. The plaintext exists in the cookie and nowhere else.
type Session struct {
	// ID is a separate random value from the token, so the row's identity is
	// never the credential itself. 32 hex chars is plenty and fits the column.
	ID string `gorm:"size:32;primaryKey"`

	UserID string `gorm:"size:64;index;not null"`

	// TokenHash is SHA-256 of the cookie value.
	TokenHash string `gorm:"size:64;uniqueIndex;not null"`

	ExpiresAt time.Time `gorm:"index;not null"`
	CreatedAt time.Time
}

type Site struct {
	ID   uint   `gorm:"primaryKey"`
	Slug string `gorm:"size:63;uniqueIndex;not null"`

	Domain string `gorm:"size:253;uniqueIndex;not null"`

	OwnerID string `gorm:"size:64;index"`

	Private bool `gorm:"not null;default:false"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

type Deployment struct {
	ID uint `gorm:"primaryKey"`

	SiteID *uint  `gorm:"index"`
	Site   *Site  `gorm:"foreignKey:SiteID;constraint:OnDelete:SET NULL"`
	Prefix string `gorm:"size:128;index;not null"`

	Status string `gorm:"size:16;index;not null;default:pending"`

	FileCount int   `gorm:"not null;default:0"`
	Bytes     int64 `gorm:"not null;default:0"`

	Manifest Manifest `gorm:"type:jsonb"`

	ClaimTokenHash string `gorm:"size:64;index"`

	// ExpiresAt is nullable so "never expires" is SQL NULL.
	//
	// It was a plain time.Time, which GORM writes as 0001-01-01 rather than
	// NULL. That made "expires_at <= now()" true for every deployment with no
	// expiry, so the sweeper's work list would have contained every permanent
	// site. A pointer makes the permanent case unrepresentable instead of
	// merely unlikely.
	ExpiresAt *time.Time `gorm:"index"`
	ClaimedAt *time.Time `gorm:"index"`
	ClaimedBy string     `gorm:"size:64"`

	// OwnerID scopes this drop to an account. Empty means an anonymous drop,
	// which is the pre-existing behaviour and stays supported.
	//
	// It is Google's stable subject id rather than a row id, matching
	// Site.OwnerID, so the same string names the account everywhere.
	OwnerID string `gorm:"size:64;index"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (d *Deployment) Expired(now time.Time) bool {
	return d.ExpiresAt != nil && d.ExpiresAt.Before(now)
}

func (d *Deployment) Claimed() bool {
	return d.ClaimedAt != nil
}

// Anonymous reports whether the drop is unclaimed and unattached to a site.
func (d *Deployment) Anonymous() bool {
	return d.SiteID == nil && !d.Claimed()
}

// PublicURLPath is where this deployment's objects are served.
func (d *Deployment) PublicURLPath() string {
	return "/" + strings.Trim(d.Prefix, "/")
}

func (d *Deployment) Slug() string {
	p := strings.Trim(d.Prefix, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (d *Deployment) BeforeCreate(tx *gorm.DB) error {
	if d.Status == "" {
		d.Status = StatusPending
	}
	return nil
}
