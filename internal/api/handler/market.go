package handler

import (
	"context"
	"net/http"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
	"github.com/gin-gonic/gin"
)

// MarketsGetter fetches a list of markets.
type MarketsGetter interface {
	GetMarkets(ctx context.Context, req *gamma.MarketsRequest) ([]gamma.Market, error)
}

type marketsQuery struct {
	Limit        *int    `form:"limit"`
	Offset       *int    `form:"offset"`
	Order        string  `form:"order"`
	Ascending    *bool   `form:"ascending"`
	Slug         string  `form:"slug"`
	SlugContains string  `form:"slug_contains"`
	Active       *bool   `form:"active"`
	Closed       *bool   `form:"closed"`
	TagSlug      string  `form:"tag_slug"`
	VolumeMin    *string `form:"volume_min"`
	VolumeMax    *string `form:"volume_max"`
	LiquidityMin *string `form:"liquidity_min"`
	LiquidityMax *string `form:"liquidity_max"`
	EndDateMin   string  `form:"end_date_min"`
	EndDateMax   string  `form:"end_date_max"`
}

// GetMarkets returns a handler that searches markets with optional filtering and pagination.
func GetMarkets(client MarketsGetter) gin.HandlerFunc {
	return func(c *gin.Context) {
		var q marketsQuery
		if err := c.ShouldBindQuery(&q); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		req := &gamma.MarketsRequest{
			Limit:        q.Limit,
			Offset:       q.Offset,
			Order:        q.Order,
			Ascending:    q.Ascending,
			Slug:         q.Slug,
			SlugContains: q.SlugContains,
			Active:       q.Active,
			Closed:       q.Closed,
			TagSlug:      q.TagSlug,
			VolumeMin:    q.VolumeMin,
			VolumeMax:    q.VolumeMax,
			LiquidityMin: q.LiquidityMin,
			LiquidityMax: q.LiquidityMax,
			EndDateMin:   q.EndDateMin,
			EndDateMax:   q.EndDateMax,
		}

		markets, err := client.GetMarkets(c.Request.Context(), req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, markets)
	}
}
