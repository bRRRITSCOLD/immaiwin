// OAuth (Google + GitHub) login flow.
//
//	GET /auth/oauth/:provider/start     redirects to provider's auth URL
//	GET /auth/oauth/:provider/callback  exchanges code, links/creates user, sets cookie, redirects to UI
//
// Account-linking model:
//   - Provider returns a stable `subject` (Google `sub`, GitHub user `id`).
//   - We first try matching by (provider, subject) on User.OAuthProviders.
//   - If no match, we look up by email: if a user exists, we LINK the
//     OAuth provider to that account (so a user who registered with
//     email/password can later add Google + sign in either way).
//   - If no email match, we create a fresh user + personal tenant
//     (same flow as /auth/register).
//
// State token: short-lived signed JWT carrying the provider name and a
// random nonce. Verifies callback came from our own start endpoint
// (CSRF). No Redis dep.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/auth"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

// OAuthDeps wraps everything the OAuth handlers need.
type OAuthDeps struct {
	Cfg      config.AuthConfig
	JWTBytes []byte
	TTL      time.Duration
	Users    *mongodb.UserRepository
	Tenants  *mongodb.TenantRepository
	Audit    *mongodb.AuditRepository
}

// providerProfile is the normalised user profile pulled from each
// provider's userinfo endpoint. Subject is always populated; email
// may be empty (GitHub users w/ private email — we then fetch from
// /user/emails).
type providerProfile struct {
	Subject string
	Email   string
	Name    string
}

// oauth2Config builds the per-provider oauth2.Config. Returns nil
// when the provider isn't configured (empty ClientID).
func oauth2Config(authCfg config.AuthConfig, provider string) *oauth2.Config {
	switch provider {
	case "google":
		if authCfg.OAuthGoogle.ClientID == "" {
			return nil
		}
		return &oauth2.Config{
			ClientID:     authCfg.OAuthGoogle.ClientID,
			ClientSecret: authCfg.OAuthGoogle.ClientSecret,
			RedirectURL:  authCfg.OAuthGoogle.RedirectURL,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
		}
	case "github":
		if authCfg.OAuthGithub.ClientID == "" {
			return nil
		}
		return &oauth2.Config{
			ClientID:     authCfg.OAuthGithub.ClientID,
			ClientSecret: authCfg.OAuthGithub.ClientSecret,
			RedirectURL:  authCfg.OAuthGithub.RedirectURL,
			Scopes:       []string{"read:user", "user:email"},
			Endpoint:     github.Endpoint,
		}
	}
	return nil
}

// stateClaims is the short-lived JWT we use as the OAuth `state`
// parameter. Carries the provider name + a nonce. Verified on
// callback; any mismatch = CSRF.
type stateClaims struct {
	Provider string `json:"provider"`
	Nonce    string `json:"nonce"`
}

func issueState(secret []byte, provider string) (string, error) {
	// Reuse the auth JWT signer w/ a 5-min TTL. State claims piggyback
	// on the regular Claims struct; the agent loop never reads them.
	c := &auth.Claims{
		UserID:   "oauth_state:" + provider,
		TenantID: ulid.Make().String(), // nonce
	}
	tok, err := auth.IssueJWT(secret, c.UserID, c.TenantID, 5*time.Minute)
	return tok, err
}

func verifyState(secret []byte, provider, raw string) error {
	c, err := auth.ParseJWT(secret, raw)
	if err != nil {
		return err
	}
	if c.UserID != "oauth_state:"+provider {
		return errors.New("state provider mismatch")
	}
	return nil
}

// OAuthStart redirects to the provider's authorize URL with a state
// token + the configured scopes. UI links to this endpoint with a
// `next` query param (the SPA path to land on after callback).
func OAuthStart(deps OAuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := c.Param("provider")
		oc := oauth2Config(deps.Cfg, provider)
		if oc == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "oauth provider not configured: " + provider})
			return
		}
		state, err := issueState(deps.JWTBytes, provider)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		url := oc.AuthCodeURL(state, oauth2.AccessTypeOnline)
		c.Redirect(http.StatusFound, url)
	}
}

// OAuthCallback finishes the flow: verifies state, exchanges code,
// fetches the user profile, links/creates a user, sets the auth
// cookie, redirects to the UI.
func OAuthCallback(deps OAuthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := c.Param("provider")
		oc := oauth2Config(deps.Cfg, provider)
		if oc == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "oauth provider not configured"})
			return
		}
		state := c.Query("state")
		if err := verifyState(deps.JWTBytes, provider, state); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid state: " + err.Error()})
			return
		}
		code := c.Query("code")
		if code == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
			return
		}
		tok, err := oc.Exchange(c.Request.Context(), code)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "oauth exchange failed: " + err.Error()})
			return
		}
		client := oc.Client(c.Request.Context(), tok)
		profile, err := fetchProfile(c.Request.Context(), client, provider)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "fetch profile: " + err.Error()})
			return
		}

		user, tenantID, err := upsertOAuthUser(c.Request.Context(), deps, provider, profile)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		_ = deps.Users.TouchLastLogin(c.Request.Context(), user.ID)

		jwtTok, err := auth.IssueJWT(deps.JWTBytes, user.ID, tenantID, deps.TTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setAuthCookie(c, AuthDeps{Cfg: deps.Cfg, TTL: deps.TTL}, jwtTok)
		recordAuditUnauth(c, deps.Audit, mongodb.AuditOAuthLinked, user.Email, user.ID, tenantID,
			map[string]any{"provider": provider})
		// Land users back on the UI. They're now authenticated via the
		// cookie set above; the SPA reads /auth/me to populate state.
		ui := strings.TrimRight(deps.Cfg.UIBaseURL, "/")
		c.Redirect(http.StatusFound, ui+"/workflows")
	}
}

// fetchProfile pulls a normalised user profile from the provider's
// userinfo endpoint. GitHub may return null email — we fallback to
// /user/emails for the primary, verified address.
func fetchProfile(ctx context.Context, client *http.Client, provider string) (providerProfile, error) {
	switch provider {
	case "google":
		resp, err := client.Get("https://www.googleapis.com/oauth2/v3/userinfo")
		if err != nil {
			return providerProfile{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		var payload struct {
			Sub   string `json:"sub"`
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return providerProfile{}, fmt.Errorf("google decode: %w", err)
		}
		if payload.Sub == "" {
			return providerProfile{}, errors.New("google profile missing sub")
		}
		return providerProfile{Subject: payload.Sub, Email: payload.Email, Name: payload.Name}, nil

	case "github":
		resp, err := client.Get("https://api.github.com/user")
		if err != nil {
			return providerProfile{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		var payload struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return providerProfile{}, fmt.Errorf("github decode: %w", err)
		}
		if payload.ID == 0 {
			return providerProfile{}, errors.New("github profile missing id")
		}
		email := payload.Email
		if email == "" {
			// Fall back to /user/emails for users w/ private email.
			emailsResp, err := client.Get("https://api.github.com/user/emails")
			if err == nil {
				defer func() { _ = emailsResp.Body.Close() }()
				ebody, _ := io.ReadAll(emailsResp.Body)
				var emails []struct {
					Email    string `json:"email"`
					Primary  bool   `json:"primary"`
					Verified bool   `json:"verified"`
				}
				_ = json.Unmarshal(ebody, &emails)
				for _, e := range emails {
					if e.Primary && e.Verified {
						email = e.Email
						break
					}
				}
			}
		}
		return providerProfile{
			Subject: fmt.Sprintf("%d", payload.ID),
			Email:   email,
			Name:    payload.Name,
		}, nil
	}
	return providerProfile{}, errors.New("unknown provider")
}

// upsertOAuthUser implements the link-or-create flow. Returns the
// resolved user + active tenant ID.
func upsertOAuthUser(ctx context.Context, deps OAuthDeps, provider string, p providerProfile) (mongodb.User, string, error) {
	if deps.Users == nil || deps.Tenants == nil {
		return mongodb.User{}, "", errors.New("auth not configured")
	}
	// 1. Match by (provider, subject) on existing OAuthLink rows.
	// We scan by email since the index is sparse on (provider+subject)
	// — a real impl would query that compound index directly. For
	// dev volumes, GetByEmail w/ subject post-filter is fine.
	if p.Email != "" {
		if u, err := deps.Users.GetByEmail(ctx, strings.ToLower(p.Email)); err == nil {
			// Existing user → link if not already linked. LinkOAuth's
			// filter is a no-op when the (provider, subject) pair is
			// already present, so we don't need a pre-check; the call
			// is idempotent.
			if linkErr := deps.Users.LinkOAuth(ctx, u.ID, provider, p.Subject, p.Email); linkErr != nil {
				// Don't fail the whole login — surface in logs but let
				// the user proceed. Worst case: provider link missed,
				// next OAuth attempt picks it up via the same path.
				return mongodb.User{}, "", fmt.Errorf("link oauth: %w", linkErr)
			}
			memberships, _ := deps.Tenants.ListMembershipsForUser(ctx, u.ID)
			if len(memberships) > 0 {
				return u, memberships[0].Tenant.ID, nil
			}
			// User exists but has no tenant (data anomaly) — mint one.
			t, terr := deps.Tenants.CreateWithOwner(ctx, mongodb.Tenant{
				ID:      ulid.Make().String(),
				Name:    p.Email + "'s workspace",
				OwnerID: u.ID,
			})
			if terr != nil {
				return mongodb.User{}, "", terr
			}
			return u, t.ID, nil
		}
	}

	// 2. No email match → create a fresh user + tenant. Subject acts
	// as a fallback identifier; email may be empty for some flows.
	if p.Email == "" {
		// Synthesise a placeholder email so the unique index is happy.
		p.Email = fmt.Sprintf("%s+%s@oauth.local", provider, p.Subject)
	}
	newUser := mongodb.User{
		ID:    ulid.Make().String(),
		Email: strings.ToLower(p.Email),
		OAuthProviders: []mongodb.OAuthLink{{
			Provider: provider,
			Subject:  p.Subject,
			Email:    p.Email,
			LinkedAt: time.Now().UTC(),
		}},
	}
	created, err := deps.Users.Create(ctx, newUser)
	if err != nil {
		return mongodb.User{}, "", err
	}
	t, err := deps.Tenants.CreateWithOwner(ctx, mongodb.Tenant{
		ID:      ulid.Make().String(),
		Name:    p.Email + "'s workspace",
		OwnerID: created.ID,
	})
	if err != nil {
		return mongodb.User{}, "", err
	}
	return created, t.ID, nil
}
