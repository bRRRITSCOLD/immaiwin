// Package auth implements the user-auth foundation: bcrypt password
// hashing, HS256 JWT issuance + verification, and the ctx-key pattern
// for plumbing the authenticated user/tenant into request handlers.
//
// JWT layout (claims):
//
//	{
//	  "sub":       "<user_id>",     // ULID
//	  "tenant_id": "<tenant_id>",   // active tenant; carries through every request
//	  "iat":       <unix>,
//	  "exp":       <unix>
//	}
//
// API keys are NOT JWTs — they live in their own collection w/ a
// hashed key value + lookup-by-prefix index. See Phase D for that.
//
// The ctx-key pattern below is how the auth middleware tells handlers
// "this request belongs to user X in tenant Y". Handlers read via
// `auth.UserFromCtx(ctx)` / `auth.TenantFromCtx(ctx)`. Both panic-free
// — they return zero values + ok=false when the ctx wasn't auth-tagged.

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// PasswordHash returns a bcrypt hash of the plaintext password using
// the default cost. Never log the plaintext; never store anything but
// the hash.
func PasswordHash(plaintext string) (string, error) {
	if len(plaintext) < 8 {
		return "", errors.New("auth: password too short (min 8)")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(hash), nil
}

// PasswordVerify constant-time-compares plaintext against a stored
// bcrypt hash. Returns true on match, false on mismatch (never errors
// for the typical wrong-password case — bcrypt's error semantics make
// callers paranoid otherwise).
func PasswordVerify(plaintext, hash string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

// Claims is the JWT payload. Tenant ID is mandatory once Phase B
// lands; this struct carries it from the start so the cookie shape
// doesn't change between phases.
type Claims struct {
	jwt.RegisteredClaims
	UserID   string `json:"sub,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
}

// IssueJWT signs a Claims payload with the configured HS256 secret.
// userID is required; tenantID is optional during Phase A (becomes
// required Phase B onward).
func IssueJWT(secret []byte, userID, tenantID string, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("auth: JWT secret not configured")
	}
	if userID == "" {
		return "", errors.New("auth: user_id required")
	}
	now := time.Now().UTC()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		UserID:   userID,
		TenantID: tenantID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(secret)
}

// ParseJWT verifies + decodes a token string. Returns the claims on
// valid + non-expired tokens. Errors otherwise (caller should map to
// 401).
func ParseJWT(secret []byte, raw string) (*Claims, error) {
	if raw == "" {
		return nil, errors.New("auth: empty token")
	}
	c := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("auth: invalid token")
	}
	return c, nil
}

// --- ctx-key plumbing ---

type ctxKey int

const (
	userCtxKey ctxKey = iota
	tenantCtxKey
)

// UserCtx is the minimal user info threaded through ctx. Avoids
// dragging the full mongo user model into every handler.
type UserCtx struct {
	ID    string
	Email string
}

// WithUser returns a child ctx tagged with the given user identity.
// Auth middleware calls this once after JWT validation.
func WithUser(parent context.Context, u UserCtx) context.Context {
	return context.WithValue(parent, userCtxKey, u)
}

// UserFromCtx pulls the user identity stamped by the auth middleware.
// ok=false when the request wasn't auth-tagged (use for endpoints
// that conditionally require auth).
func UserFromCtx(ctx context.Context) (UserCtx, bool) {
	v, ok := ctx.Value(userCtxKey).(UserCtx)
	return v, ok
}

// WithTenant tags the active tenant for this request. Same semantics
// as WithUser — middleware sets, handlers read.
func WithTenant(parent context.Context, tenantID string) context.Context {
	return context.WithValue(parent, tenantCtxKey, tenantID)
}

// TenantFromCtx pulls the active tenant ID. Empty string + ok=false
// when not set. Phase B+ handlers should treat this as the only
// allowed tenant scope for the request.
func TenantFromCtx(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(tenantCtxKey).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
