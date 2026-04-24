package handler

import (
	"context"
	"net/http"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
	"github.com/gin-gonic/gin"
)

// EventsGetter fetches a list of events with their nested markets.
type EventsGetter interface {
	GetEvents(ctx context.Context, req *gamma.EventsRequest) ([]gamma.Event, error)
}

type eventsQuery struct {
	Limit      *int   `form:"limit"`
	Offset     *int   `form:"offset"`
	Order      string `form:"order"`
	Ascending  *bool  `form:"ascending"`
	Active     *bool  `form:"active"`
	Closed     *bool  `form:"closed"`
	EndDateMin string `form:"end_date_min"`
}

// GetEvents returns a handler that fetches events with optional filtering and pagination.
func GetEvents(client EventsGetter) gin.HandlerFunc {
	return func(c *gin.Context) {
		var q eventsQuery
		if err := c.ShouldBindQuery(&q); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		req := &gamma.EventsRequest{
			Limit:      q.Limit,
			Offset:     q.Offset,
			Ascending:  q.Ascending,
			Active:     q.Active,
			Closed:     q.Closed,
			EndDateMin: q.EndDateMin,
		}
		if q.Order != "" {
			req.Order = []string{q.Order}
		}

		events, err := client.GetEvents(c.Request.Context(), req)
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
