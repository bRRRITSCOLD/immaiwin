package handler

import (
	"context"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/watchlist"
	"github.com/gin-gonic/gin"
)

// WatchlistStore reads and syncs the watchlist.
type WatchlistStore interface {
	List(ctx context.Context) ([]watchlist.WatchlistItem, error)
	Sync(ctx context.Context, items []watchlist.WatchlistItem) error
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
