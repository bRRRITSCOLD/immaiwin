package handler

import (
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// ConnectionOAuthURL returns the OAuth authorize URL + callback URL for a connection.
// GET /api/v1/connections/:id/oauth/url
func ConnectionOAuthURL(store ConnectionStore, db *mongo.Database, baseURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		conn, err := store.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
			return
		}

		switch conn.Type {
		case workflow.ConnectionTypeSchwab:
			tm := workflow.BuildSchwabTokenManager(conn.Config, db, conn.ID)
			authorizeURL := tm.AuthorizeURL(conn.ID)
			callbackURL := baseURL + "/auth/connections/" + conn.ID + "/callback"
			c.JSON(http.StatusOK, gin.H{
				"authorize_url": authorizeURL,
				"callback_url":  callbackURL,
			})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "connection type does not support OAuth"})
		}
	}
}

// ConnectionOAuthCallback handles the OAuth callback for any connection.
// GET /auth/connections/:id/callback
func ConnectionOAuthCallback(store ConnectionStore, db *mongo.Database) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		conn, err := store.GetByID(c.Request.Context(), id)
		if err != nil {
			c.Data(http.StatusNotFound, "text/html; charset=utf-8",
				[]byte(`<html><body><h3>Connection not found</h3></body></html>`))
			return
		}

		code := c.Query("code")
		if code == "" {
			c.Data(http.StatusBadRequest, "text/html; charset=utf-8",
				[]byte(`<html><body><h3>Missing authorization code</h3></body></html>`))
			return
		}

		switch conn.Type {
		case workflow.ConnectionTypeSchwab:
			tm := workflow.BuildSchwabTokenManager(conn.Config, db, conn.ID)
			if err := tm.ExchangeCode(c.Request.Context(), code); err != nil {
				c.Data(http.StatusInternalServerError, "text/html; charset=utf-8",
					[]byte(`<html><body><h3>Token exchange failed: `+err.Error()+`</h3></body></html>`))
				return
			}
		default:
			c.Data(http.StatusBadRequest, "text/html; charset=utf-8",
				[]byte(`<html><body><h3>Connection type does not support OAuth</h3></body></html>`))
			return
		}

		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!DOCTYPE html>
<html><head><title>Authorization Complete</title></head>
<body style="font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#0a0a0a;color:#fafafa">
<div style="text-align:center">
<h2>Authorization successful</h2>
<p>You can close this window.</p>
<script>setTimeout(function(){window.close()},1500)</script>
</div>
</body></html>`))
	}
}

// ConnectionOAuthStatus returns whether OAuth is complete for a connection.
// GET /api/v1/connections/:id/oauth/status
func ConnectionOAuthStatus(store ConnectionStore, db *mongo.Database) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		conn, err := store.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
			return
		}

		switch conn.Type {
		case workflow.ConnectionTypeSchwab:
			tm := workflow.BuildSchwabTokenManager(conn.Config, db, conn.ID)
			if err := tm.Load(c.Request.Context()); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"authorized": tm.IsAuthorized()})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "connection type does not support OAuth"})
		}
	}
}
