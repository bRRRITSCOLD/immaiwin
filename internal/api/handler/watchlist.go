package handler

import (
	"context"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/watchlist"
	"github.com/gin-gonic/gin"
)

// WatchlistStore reads and syncs the watchlist.
type WatchlistStore interface {
	List(ctx context.Context) ([]watchlist.WatchlistItem, error)
	Sync(ctx context.Context, items []watchlist.WatchlistItem) error
	UpdateConfig(ctx context.Context, marketID, exprStr string, windowSize int) error
}

// GetWatchlist returns all watchlisted markets.
func GetWatchlist(store WatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := store.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if items == nil {
			items = []watchlist.WatchlistItem{}
		}
		c.JSON(http.StatusOK, items)
	}
}

type watchlistSyncInput struct {
	MarketID string `json:"market_id"`
	Question string `json:"question"`
	Slug     string `json:"slug"`
}

// SyncWatchlist replaces the watchlist with the provided set of markets.
// Send an empty array to clear the watchlist.
func SyncWatchlist(store WatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input []watchlistSyncInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		items := make([]watchlist.WatchlistItem, len(input))
		for i, v := range input {
			items[i] = watchlist.WatchlistItem{
				MarketID: v.MarketID,
				Question: v.Question,
				Slug:     v.Slug,
			}
		}

		if err := store.Sync(c.Request.Context(), items); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

type updateConfigInput struct {
	Expr       string `json:"expr"`
	WindowSize int    `json:"window_size"` // 0 = use global default
}

// UpdateWatchlistConfig validates and saves a custom unusual-trade expression and window
// size for a market. An empty expr clears the expression (falls back to default detector).
// A window_size of 0 resets to the global default (POLYMARKET_WINDOW_SIZE env var).
func UpdateWatchlistConfig(store WatchlistStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		marketID := c.Param("market_id")
		var input updateConfigInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.Expr != "" {
			if _, err := polymarket.CompileExpr(input.Expr); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expression: " + err.Error()})
				return
			}
		}
		if input.WindowSize < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "window_size must be >= 0"})
			return
		}
		if err := store.UpdateConfig(c.Request.Context(), marketID, input.Expr, input.WindowSize); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
