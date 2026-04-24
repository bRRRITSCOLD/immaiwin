package handler

import (
	"context"
	"net/http"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
	"github.com/gin-gonic/gin"
)

// EventsSearcher searches events via the Polymarket /public-search endpoint.
type EventsSearcher interface {
	SearchEvents(ctx context.Context, req *gamma.PublicSearchRequest) ([]gamma.Event, error)
}

type searchEventsQuery struct {
	Q      string `form:"q"`
	Status string `form:"status"`
	Page   *int   `form:"page"`
	Sort   string `form:"sort"`
}

// eventSortFieldName maps UI sort values to Polymarket PublicSearch sort values.
var eventSortFieldName = map[string]string{
	"volume_num":     "volume",
	"volume24hr_num": "volume24hr",
	"liquidity_num":  "liquidity",
	"end_date":       "end_date",
}

const eventsSearchPageSize = 20

// SearchEvents returns a handler that searches events by free-text query via /public-search.
func SearchEvents(client EventsSearcher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var q searchEventsQuery
		if err := c.ShouldBindQuery(&q); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		page := 1
		if q.Page != nil && *q.Page > 0 {
			page = *q.Page
		}
		limit := eventsSearchPageSize

		req := &gamma.PublicSearchRequest{
			Query:        q.Q,
			LimitPerType: &limit,
			Page:         &page,
		}

		switch q.Status {
		case "active":
			req.EventsStatus = "active"
		case "closed":
			req.EventsStatus = "closed"
		}

		if apiField, ok := eventSortFieldName[q.Sort]; ok {
			req.Sort = apiField
		}

		events, err := client.SearchEvents(c.Request.Context(), req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if events == nil {
			events = []gamma.Event{}
		}
		c.JSON(http.StatusOK, events)
	}
}
