package schwab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	authBaseURL  = "https://api.schwabapi.com/v1/oauth"
	tokenDocID   = "schwab_tokens"
	refreshBuffer = 5 * time.Minute // refresh access token this far before expiry
)

// tokenDoc is the MongoDB document that stores OAuth tokens.
type tokenDoc struct {
	ID           string    `bson:"_id"`
	AccessToken  string    `bson:"access_token"`
	RefreshToken string    `bson:"refresh_token"`
	ExpiresAt    time.Time `bson:"expires_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

// TokenManager handles OAuth2 token lifecycle: authorize, exchange, refresh, storage.
type TokenManager struct {
	cfg  config.SchwabConfig
	col  *mongo.Collection
	http *http.Client

	mu          sync.RWMutex
	accessToken string
	expiresAt   time.Time
}

func NewTokenManager(cfg config.SchwabConfig, db *mongo.Database) *TokenManager {
	return &TokenManager{
		cfg:  cfg,
		col:  db.Collection("schwab_tokens"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// AuthorizeURL returns the Schwab OAuth2 authorization URL to redirect the user to.
func (m *TokenManager) AuthorizeURL(state string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", m.cfg.ClientID)
	v.Set("redirect_uri", m.cfg.CallbackURL)
	if state != "" {
		v.Set("state", state)
	}
	return authBaseURL + "/authorize?" + v.Encode()
}

// ExchangeCode exchanges an authorization code for access + refresh tokens.
func (m *TokenManager) ExchangeCode(ctx context.Context, code string) error {
	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("code", code)
	body.Set("redirect_uri", m.cfg.CallbackURL)

	resp, err := m.postToken(ctx, body)
	if err != nil {
		return err
	}
	return m.save(ctx, resp)
}

// Load restores tokens from MongoDB into memory. Called at startup.
func (m *TokenManager) Load(ctx context.Context) error {
	var doc tokenDoc
	err := m.col.FindOne(ctx, bson.M{"_id": tokenDocID}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil // not yet authorized — ok
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.accessToken = doc.AccessToken
	m.expiresAt = doc.ExpiresAt
	m.mu.Unlock()
	return nil
}

// Disconnect clears tokens from memory and deletes the MongoDB document.
func (m *TokenManager) Disconnect(ctx context.Context) error {
	m.mu.Lock()
	m.accessToken = ""
	m.expiresAt = time.Time{}
	m.mu.Unlock()
	_, err := m.col.DeleteOne(ctx, bson.M{"_id": tokenDocID})
	return err
}

// IsAuthorized returns true if a non-empty access token is stored in memory.
func (m *TokenManager) IsAuthorized() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accessToken != ""
}

// AccessToken returns a valid access token, refreshing if necessary.
func (m *TokenManager) AccessToken(ctx context.Context) (string, error) {
	m.mu.RLock()
	tok := m.accessToken
	exp := m.expiresAt
	m.mu.RUnlock()

	if tok == "" {
		return "", fmt.Errorf("schwab: not authorized — visit /auth/schwab to authorize")
	}
	if time.Until(exp) > refreshBuffer {
		return tok, nil
	}
	return m.refresh(ctx)
}

// RunRefresher starts a background goroutine that proactively refreshes the access token.
func (m *TokenManager) RunRefresher(ctx context.Context) {
	go func() {
		t := time.NewTicker(20 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := m.refresh(ctx); err != nil {
					slog.Error("schwab: token refresh failed", "err", err)
				}
			}
		}
	}()
}

func (m *TokenManager) refresh(ctx context.Context) (string, error) {
	var doc tokenDoc
	if err := m.col.FindOne(ctx, bson.M{"_id": tokenDocID}).Decode(&doc); err != nil {
		return "", fmt.Errorf("schwab refresh: load tokens: %w", err)
	}

	body := url.Values{}
	body.Set("grant_type", "refresh_token")
	body.Set("refresh_token", doc.RefreshToken)

	resp, err := m.postToken(ctx, body)
	if err != nil {
		return "", err
	}
	if err := m.save(ctx, resp); err != nil {
		return "", err
	}

	m.mu.RLock()
	tok := m.accessToken
	m.mu.RUnlock()
	return tok, nil
}

func (m *TokenManager) postToken(ctx context.Context, body url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.schwabapi.com/v1/oauth/token",
		strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}
	creds := base64.StdEncoding.EncodeToString([]byte(m.cfg.ClientID + ":" + m.cfg.ClientSecret))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("schwab token: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("schwab: close token response body", "err", err)
		}
	}()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schwab token: HTTP %d", res.StatusCode)
	}
	var tr tokenResponse
	if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

func (m *TokenManager) save(ctx context.Context, tr *tokenResponse) error {
	exp := time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second)
	doc := tokenDoc{
		ID:           tokenDocID,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    exp,
		UpdatedAt:    time.Now().UTC(),
	}
	_, err := m.col.UpdateOne(ctx,
		bson.M{"_id": tokenDocID},
		bson.M{"$set": doc},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("schwab: save tokens: %w", err)
	}
	m.mu.Lock()
	m.accessToken = tr.AccessToken
	m.expiresAt = exp
	m.mu.Unlock()
	slog.Info("schwab: tokens saved", "expires_at", exp)
	return nil
}
