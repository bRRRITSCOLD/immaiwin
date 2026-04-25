package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// SchwabAuthorizer handles the Schwab OAuth2 flow.
type SchwabAuthorizer interface {
	AuthorizeURL(state string) string
	ExchangeCode(ctx context.Context, code string) error
	IsAuthorized() bool
	Disconnect(ctx context.Context) error
}

// SchwabAuthorize redirects the browser to Schwab's OAuth2 consent page.
func SchwabAuthorize(auth SchwabAuthorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		url := auth.AuthorizeURL("immaiwin")
		c.Redirect(http.StatusFound, url)
	}
}

// SchwabCallback handles the OAuth2 callback, exchanges the code for tokens.
func SchwabCallback(auth SchwabAuthorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Query("code")
		if code == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
			return
		}
		if err := auth.ExchangeCode(c.Request.Context(), code); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "authorized"})
	}
}

// SchwabStatus returns whether Schwab tokens are loaded.
func SchwabStatus(auth SchwabAuthorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"authorized": auth.IsAuthorized()})
	}
}

// SchwabDisconnect clears stored tokens.
func SchwabDisconnect(auth SchwabAuthorizer) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := auth.Disconnect(c.Request.Context()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "disconnected"})
	}
}
