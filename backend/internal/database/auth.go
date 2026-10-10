package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrNoSession means the presented cookie names no live session. It is the
// normal answer for a signed-out or expired browser, not a failure.
var ErrNoSession = errors.New("no such session")

// UpsertUser records an account after Google has vouched for it.
//
// The subject id is the primary key, so a returning user updates in place. Name
// and picture are refreshed every time because Google is the source of truth
// for them and they change without warning.
func (d *Database) UpsertUser(ctx context.Context, u User) (*User, error) {
	if u.ID == "" {
		return nil, errors.New("user has no id")
	}

	existing, err := d.getUser(ctx, u.ID)
	switch {
	case err == nil:
		existing.Email = u.Email
		existing.EmailVerified = u.EmailVerified
		existing.Name = u.Name
		existing.Picture = u.Picture
		if err := d.db.WithContext(ctx).Save(existing).Error; err != nil {
			return nil, fmt.Errorf("update user: %w", err)
		}
		return existing, nil
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, err
	}

	if err := d.db.WithContext(ctx).Create(&u).Error; err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return &u, nil
}

func (d *Database) getUser(ctx context.Context, id string) (*User, error) {
	var u User
	err := d.db.WithContext(ctx).First(&u, "id = ?", id).Error
	return &u, err
}

// GetUser returns an account by id, or ErrNotFound.
func (d *Database) GetUser(ctx context.Context, id string) (*User, error) {
	u, err := d.getUser(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// NewSession creates a session and returns the plaintext token exactly once.
//
// The token is 256 bits of randomness. Only its hash reaches the database, so a
// table copy cannot be replayed as a cookie. The row id is separate and shorter,
// so a leaked id identifies nothing.
func (d *Database) NewSession(ctx context.Context, userID string, lifetime time.Duration) (string, *Session, error) {
	token, err := randomToken()
	if err != nil {
		return "", nil, err
	}

	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", nil, fmt.Errorf("read random: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])

	s := Session{
		ID:        id,
		UserID:    userID,
		TokenHash: hashToken(token),
		ExpiresAt: time.Now().UTC().Add(lifetime),
		CreatedAt: time.Now().UTC(),
	}
	if err := d.db.WithContext(ctx).Create(&s).Error; err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}
	return token, &s, nil
}

// SessionUser resolves a cookie value to the account it belongs to.
//
// An unknown hash and an expired row are deliberately indistinguishable, both
// returning ErrNoSession: telling them apart would let a caller probe which
// cookies are real.
func (d *Database) SessionUser(ctx context.Context, token string, now time.Time) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}

	var s Session
	err := d.db.WithContext(ctx).
		Where("token_hash = ?", hashToken(token)).
		First(&s).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}

	if !s.ExpiresAt.After(now.UTC()) {
		// Drop it on the way past so the table does not accumulate dead rows.
		_ = d.db.WithContext(ctx).Delete(&Session{}, "id = ?", s.ID).Error
		return nil, ErrNoSession
	}

	user, err := d.getUser(ctx, s.UserID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	return user, nil
}

// DeleteSession ends one session. Used by sign-out, where the caller knows the
// cookie it is retiring.
func (d *Database) DeleteSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return d.db.WithContext(ctx).Delete(&Session{}, "token_hash = ?", hashToken(token)).Error
}

// UsageBytes is what an account currently stores: the bytes of its live
// deployments.
//
// Only live rows count. A redeployed site's superseded deployment has had its
// objects removed by the time the new one goes live, so counting it would bill
// for files that no longer exist. A deployment mid-replacement is excluded
// rather than double counted.
func (d *Database) UsageBytes(ctx context.Context, ownerID string) (int64, error) {
	var total int64
	err := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("owner_id = ? AND status = ?", ownerID, StatusLive).
		Select("COALESCE(SUM(bytes), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("usage for %q: %w", ownerID, err)
	}
	return total, nil
}

// UsageBySite is the live byte total for one site, used to check a redeploy
// against the owner's remaining quota before anything is written.
func (d *Database) UsageBySite(ctx context.Context, siteID uint) (int64, error) {
	var total int64
	err := d.db.WithContext(ctx).
		Model(&Deployment{}).
		Where("site_id = ? AND status = ?", siteID, StatusLive).
		Select("COALESCE(SUM(bytes), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("site usage %d: %w", siteID, err)
	}
	return total, nil
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
