// Approval magic-link tokens — short-lived signed tokens that bind a
// pending approval to a single redeem attempt. The token IS the auth
// for the magic-link endpoint, so leaking one is equivalent to handing
// out the approve/reject capability for that run. Tokens expire (24h
// default) and carry a `jti` (PendingApprovalState.TokenID) that the
// redeem handler matches against the persisted run record — once the
// run leaves `pending_approval` the TokenID is cleared, so a stale
// link can't resurrect a closed gate.
//
// Decision (approve | reject) is NOT bound into the token. The
// recipient's landing page sends both the token and the chosen
// decision; this keeps a single link useful for either action and
// matches the UX of n8n / Zapier approval emails.

package workflow

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ApprovalTokenIssuer is the JWT `iss` claim for magic-link approval
// tokens. Distinct from the user-session `iss` so a leaked session JWT
// can't be replayed against the approval endpoint and vice-versa.
const ApprovalTokenIssuer = "burrow-approval"

// DefaultApprovalTokenTTL is the token validity window. 24h is long
// enough to cover an overnight pending approval but short enough that
// a leaked token decays without operator intervention.
const DefaultApprovalTokenTTL = 24 * time.Hour

// ApprovalClaims is the body of a magic-link approval token.
type ApprovalClaims struct {
	jwt.RegisteredClaims
}

// SignApprovalToken produces a signed magic-link token for (runID,
// tokenID). `secret` is the same JWT-signing secret the user-session
// path uses (`AuthConfig.JWTSecret`); reusing it keeps deployments from
// having to manage a second key. `ttl <= 0` falls back to
// DefaultApprovalTokenTTL.
func SignApprovalToken(secret []byte, runID, tokenID string, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("approval token: signing secret empty")
	}
	if runID == "" || tokenID == "" {
		return "", errors.New("approval token: runID and tokenID required")
	}
	if ttl <= 0 {
		ttl = DefaultApprovalTokenTTL
	}
	now := time.Now().UTC()
	claims := ApprovalClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    ApprovalTokenIssuer,
			Subject:   runID,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(secret)
}

// VerifyApprovalToken parses + validates a magic-link token. Returns
// the claims (run_id via `Subject`, token_id via `ID`) on success.
// Caller is responsible for matching the returned token_id against the
// run record's `PendingApproval.TokenID`; this function only enforces
// signature + expiry + issuer.
func VerifyApprovalToken(secret []byte, raw string) (*ApprovalClaims, error) {
	if len(secret) == 0 {
		return nil, errors.New("approval token: verifying secret empty")
	}
	claims := &ApprovalClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("approval token: unexpected signing method %v", t.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("approval token: %w", err)
	}
	if !tok.Valid {
		return nil, errors.New("approval token: invalid")
	}
	if claims.Issuer != ApprovalTokenIssuer {
		return nil, fmt.Errorf("approval token: wrong issuer %q (expected %q)", claims.Issuer, ApprovalTokenIssuer)
	}
	if claims.Subject == "" || claims.ID == "" {
		return nil, errors.New("approval token: missing run_id or token_id")
	}
	return claims, nil
}
