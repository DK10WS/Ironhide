package core

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/dk10ws/ironhide/internal/database"
)

// claimedByToken records who claimed a deployment when the token is the only
// credential available. It is a fixed string rather than the token itself, so
// no secret reaches the database or the logs.
const claimedByToken = "claim-token"

// Claim redeems a claim token, making an anonymous drop permanent.
//
// The token is the entire credential: 256 bits of CSPRNG output, single use,
// and only its hash is stored. A spent token and one that never existed are
// indistinguishable, so a caller cannot use the answer to discover which tokens
// are outstanding.
func (c *Core) Claim(ctx context.Context, token string) (Claim, error) {
	if token == "" {
		return Claim{}, fmt.Errorf("%w: missing claim token", database.ErrNotFound)
	}

	dep, err := c.db.ClaimPermanently(ctx, token, claimedByToken)
	if err != nil {
		return Claim{}, err
	}

	slog.Info("deployment claimed", "id", dep.ID, "prefix", dep.Prefix)

	out := Claim{
		ID:  dep.ID,
		URL: c.PreviewURL(dep.Slug()),
	}
	if dep.ClaimedAt != nil {
		out.ClaimedAt = *dep.ClaimedAt
	}
	return out, nil
}
