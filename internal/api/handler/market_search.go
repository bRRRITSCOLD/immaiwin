package handler

import (
	"context"
	"net/http"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
	"github.com/gin-gonic/gin"
)

// MarketsSearcher searches markets via the /markets endpoint with slug_contains support.
type MarketsSearcher interface {
	SearchMarkets(ctx context.Context, req *gamma.MarketsRequest) ([]gamma.Market, error)
}

type searchQuery struct {
	Q          string `form:"q"`
	Status     string `form:"status"`
	Page       *int   `form:"page"`
	Sort       string `form:"sort"`
	Ascending  *bool  `form:"ascending"`
	EndDateMin string `form:"end_date_min"`
}

// sortFieldName maps UI sort values to Polymarket API order param values.
// The API uses "volume"/"liquidity" not the "_num" variants for the order param.
var sortFieldName = map[string]string{
	"volume_num":     "volume",
	"volume24hr_num": "volume24hr",
	"liquidity_num":  "liquidity",
	"end_date":       "end_date",
}

const searchPageSize = 20

// SearchMarkets returns a handler that searches markets by free-text query using
// a direct call to the Polymarket /markets endpoint with slug_contains.
func SearchMarkets(client MarketsSearcher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var q searchQuery
		if err := c.ShouldBindQuery(&q); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		page := 1
		if q.Page != nil && *q.Page > 0 {
			page = *q.Page
		}
		limit := searchPageSize
		offset := (page - 1) * searchPageSize

		req := &gamma.MarketsRequest{
			SlugContains: q.Q,
			Limit:        &limit,
			Offset:       &offset,
			EndDateMin:   q.EndDateMin,
		}

		t := true
		switch q.Status {
		case "active":
			req.Active = &t
		case "closed":
			req.Closed = &t
		}

		if apiField, ok := sortFieldName[q.Sort]; ok {
			req.Order = apiField
			if q.Ascending != nil {
				req.Ascending = q.Ascending
			} else {
				f := false
				req.Ascending = &f
			}
		}

		markets, err := client.SearchMarkets(c.Request.Context(), req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, markets)
	}
}
