// Password reset endpoints — covers the "forgot password" flow.
//
//	POST /api/v1/auth/password_reset/request   { email }
//	POST /api/v1/auth/password_reset/confirm   { token, new_password }
//
// Design:
//   - Request always returns 200 (no enumeration). If the email
//     belongs to a user, mint a 15-min purpose=password_reset JWT and
//     hand it to the email Sender. Otherwise silently no-op.
//   - The token's hash is recorded in Redis with a 15-min TTL so
//     Confirm can enforce single-use: the first valid Confirm DELs
//     the key; any reuse fails.
//   - Confirm verifies signature + exp + Purpose==password_reset, then
//     swaps the user's bcrypt hash via UserRepository.UpdatePasswordHash.
//   - Rate limited via the same /auth/* middleware so a single IP
//     can't enumerate addresses by spamming Request.

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/email"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/gin-gonic/gin"
)

// PasswordResetDeps wraps the handler's needs.
type PasswordResetDeps struct {
	Users     *mongodb.UserRepository
	Audit     *mongodb.AuditRepository
	JWTBytes  []byte
	UIBaseURL string
	Email     email.Sender
	Redis     *rediss.Client // for single-use token tracking
	TokenTTL  time.Duration  // default 15min
}

// passwordResetTokenTTL is the default lifetime of a reset link. Short
// because the link gives full account-takeover power if intercepted.
const passwordResetTokenTTL = 15 * time.Minute

// tokenHashKey is the Redis key holding a SHA256 of the issued reset
// token. Existence == valid+unused; deletion on Confirm enforces
// single-use semantics.
func tokenHashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "auth:password_reset:" + hex.EncodeToString(sum[:])
}

// PasswordResetRequest accepts {email}, issues a token if the email is
// known, and dispatches an email. Always 200 — no leak.
func PasswordResetRequest(deps PasswordResetDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))
		// Always respond 200 to avoid email enumeration. The work below
		// is "best effort" — log internal errors but don't surface.
		c.JSON(http.StatusOK, gin.H{"ok": true})
		recordAuditUnauth(c, deps.Audit, mongodb.AuditPasswordResetRequest, req.Email, "", "", nil)

		go dispatchPasswordResetEmail(deps, req.Email)
	}
}

// dispatchPasswordResetEmail runs out-of-band so the HTTP response
// returns immediately regardless of email-sender latency. Uses a
// fresh ctx (request ctx is already done).
func dispatchPasswordResetEmail(deps PasswordResetDeps, lowerEmail string) {
	if lowerEmail == "" || deps.Users == nil || deps.Email == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	u, err := deps.Users.GetByEmail(ctx, lowerEmail)
	if err != nil {
		// User doesn't exist — silent. Don't leak via logs at info
		// level either; debug only.
		slog.Debug("password_reset: no user for email", "email", lowerEmail)
		return
	}

	ttl := deps.TokenTTL
	if ttl <= 0 {
		ttl = passwordResetTokenTTL
	}

	tok, err := auth.IssuePurposeJWT(deps.JWTBytes, u.ID, auth.PurposePasswordReset, ttl)
	if err != nil {
		slog.Warn("password_reset: token issue failed", "user_id", u.ID, "err", err)
		return
	}

	// Track in redis for single-use enforcement. Failure here = degrade
	// to "may be used multiple times until expiry" rather than abort.
	if deps.Redis != nil {
		if err := deps.Redis.Set(ctx, tokenHashKey(tok), u.ID, ttl); err != nil {
			slog.Warn("password_reset: redis Set for single-use failed", "err", err)
		}
	}

	resetURL := fmt.Sprintf("%s/reset?token=%s", strings.TrimRight(deps.UIBaseURL, "/"), tok)
	// Dev convenience: log the URL on its own line so the operator can
	// copy/paste cleanly from the terminal. The body-embedded version
	// gets line-wrapped by slog's text handler when fields are long,
	// which corrupts the JWT (3 dot-separated segments).
	slog.Info("password_reset: link issued", "user_id", u.ID, "reset_url", resetURL, "ttl", ttl)
	body := fmt.Sprintf(
		"A password reset was requested for your burrow account.\n\n"+
			"Click the link below to set a new password (valid for %s):\n\n%s\n\n"+
			"If you didn't request this, ignore this email — your password is unchanged.",
		ttl, resetURL,
	)
	if err := deps.Email.Send(ctx, email.Message{
		To:      u.Email,
		Subject: "Reset your burrow password",
		Body:    body,
	}); err != nil {
		slog.Warn("password_reset: email Send failed", "user_id", u.ID, "err", err)
	}
}

// PasswordResetConfirm accepts {token, new_password}. Token must
// validate as a Purpose==password_reset JWT, must still exist in
// Redis (single-use), and the new password must pass minimum-length.
// On success the bcrypt hash is replaced + the Redis key deleted.
func PasswordResetConfirm(deps PasswordResetDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Token       string `json:"token"`
			NewPassword string `json:"new_password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Token == "" || req.NewPassword == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "token + new_password required"})
			return
		}

		claims, err := auth.ParseJWT(deps.JWTBytes, req.Token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		if claims.Purpose != auth.PurposePasswordReset {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "token not a password-reset token"})
			return
		}

		// Single-use enforcement: the redis key must exist. If it's
		// gone, the token has either been used already or its TTL
		// expired (past 15min). Either way, reject.
		if deps.Redis != nil {
			n, err := deps.Redis.Exists(c.Request.Context(), tokenHashKey(req.Token))
			if err != nil {
				slog.Warn("password_reset: redis Exists failed (failing closed)", "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "verification unavailable, retry"})
				return
			}
			if n == 0 {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "token already used or expired"})
				return
			}
		}

		newHash, err := auth.PasswordHash(req.NewPassword)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := deps.Users.UpdatePasswordHash(c.Request.Context(), claims.UserID, newHash); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Burn the token so it can't be reused. Best-effort — even if
		// this fails, the next attempt will see the original Set's TTL
		// expire eventually.
		if deps.Redis != nil {
			if _, err := deps.Redis.Del(c.Request.Context(), tokenHashKey(req.Token)); err != nil {
				slog.Warn("password_reset: redis Del failed (token still bound to TTL)", "err", err)
			}
		}
		recordAuditUnauth(c, deps.Audit, mongodb.AuditPasswordResetConfirm, "", claims.UserID, "", nil)

		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
