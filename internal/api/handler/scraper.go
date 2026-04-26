package handler

import (
	"context"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/gin-gonic/gin"
)

// ScraperConfigStore is the storage interface for scraper configs.
type ScraperConfigStore interface {
	List(ctx context.Context) ([]news.ScraperConfig, error)
	GetOrDefault(ctx context.Context, source, defaultFeedURL string) (news.ScraperConfig, error)
	Upsert(ctx context.Context, cfg news.ScraperConfig) error
	ClearScript(ctx context.Context, source string) error
	Delete(ctx context.Context, source string) error
}

// ListScraperConfigs returns all stored scraper configurations.
func ListScraperConfigs(store ScraperConfigStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfgs, err := store.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if cfgs == nil {
			cfgs = []news.ScraperConfig{}
		}
		c.JSON(http.StatusOK, cfgs)
	}
}

// PatchScraperConfig updates a scraper config's script and/or feed_url.
// The script is validated (must compile + define parse(raw)) before saving.
func PatchScraperConfig(store ScraperConfigStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		source := c.Param("source")
		if source == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source required"})
			return
		}

		var body struct {
			Script  *string `json:"script"`
			FeedURL *string `json:"feed_url"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if body.Script != nil && *body.Script != "" {
			if err := news.CompileScript(*body.Script); err != nil {
				c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid script: " + err.Error()})
				return
			}
		}

		cfg := news.ScraperConfig{Source: source}
		if body.Script != nil {
			cfg.Script = *body.Script
		}
		if body.FeedURL != nil {
			cfg.FeedURL = *body.FeedURL
		}

		if err := store.Upsert(c.Request.Context(), cfg); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, cfg)
	}
}

// DeleteScraperScript clears the custom script for a source, reverting to default parser.
func DeleteScraperScript(store ScraperConfigStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		source := c.Param("source")
		if source == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source required"})
			return
		}
		if err := store.ClearScript(c.Request.Context(), source); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// DeleteScraperConfig removes a scraper config entirely.
func DeleteScraperConfig(store ScraperConfigStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		source := c.Param("source")
		if source == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source required"})
			return
		}
		if err := store.Delete(c.Request.Context(), source); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// ValidateScript checks a script without saving it. Returns 200 on success.
func ValidateScript() gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Script string `json:"script" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := news.CompileScript(body.Script); err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
