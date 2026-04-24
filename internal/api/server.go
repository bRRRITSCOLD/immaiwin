package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/api/handler"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

type Server struct {
	cfg         config.APIConfig
	broadcaster *rediss.Broadcaster
	server      *http.Server
}

type marketsClient interface {
	handler.MarketsGetter
	handler.MarketsSearcher
	handler.EventsGetter
	handler.EventsSearcher
}

func NewServer(cfg config.APIConfig, rc *rediss.Client, pm marketsClient, wl handler.WatchlistStore, tr handler.TradesLister) *Server {
	b := rediss.NewBroadcaster(rc)

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), cors.Default())

	r.GET("/health", handler.Health)
	r.GET("/api/v1/trades/stream", handler.StreamTrades(tr, b))
	r.GET("/api/v1/trades", handler.GetTrades(tr))
	r.GET("/api/v1/markets", handler.GetMarkets(pm))
	r.GET("/api/v1/markets/search", handler.SearchMarkets(pm))
	r.GET("/api/v1/events", handler.GetEvents(pm))
	r.GET("/api/v1/events/search", handler.SearchEvents(pm))
	r.GET("/api/v1/watchlist", handler.GetWatchlist(wl))
	r.PUT("/api/v1/watchlist", handler.SyncWatchlist(wl))

	return &Server{
		cfg:         cfg,
		broadcaster: b,
		server: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			Handler: r,
		},
	}
}

// Start launches the Redis subscriber broadcaster then serves HTTP.
// It blocks until the server closes.
func (s *Server) Start(ctx context.Context) error {
	go s.broadcaster.Run(ctx)
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) Addr() string {
	return s.server.Addr
}
