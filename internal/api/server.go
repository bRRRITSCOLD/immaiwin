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
	cfg                config.APIConfig
	broadcaster        *rediss.Broadcaster
	newsBroadcaster    *rediss.Broadcaster
	optsBroadcaster    *rediss.Broadcaster
	futuresBroadcaster *rediss.Broadcaster
	server             *http.Server
}

type marketsClient interface {
	handler.MarketsGetter
	handler.MarketsSearcher
	handler.EventsGetter
	handler.EventsSearcher
}

func NewServer(
	cfg config.APIConfig,
	rc *rediss.Client,
	pm marketsClient,
	wl handler.WatchlistStore,
	tr handler.TradesLister,
	nr handler.NewsLister,
	auth handler.SchwabAuthorizer,
	owl handler.OptionsWatchlistStore,
	fwl handler.FuturesWatchlistStore,
	sc handler.ScraperConfigStore,
) *Server {
	b := rediss.NewBroadcaster(rc, rediss.TradesChannel)
	nb := rediss.NewBroadcaster(rc, rediss.NewsChannel)
	ob := rediss.NewBroadcaster(rc, rediss.OptionsChannel)
	fb := rediss.NewBroadcaster(rc, rediss.FuturesChannel)

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), cors.Default())

	r.GET("/health", handler.Health)

	// Auth
	r.GET("/auth/schwab", handler.SchwabAuthorize(auth))
	r.GET("/auth/schwab/callback", handler.SchwabCallback(auth))
	r.GET("/api/v1/auth/schwab/status", handler.SchwabStatus(auth))
	r.DELETE("/api/v1/auth/schwab", handler.SchwabDisconnect(auth))

	// Trades (Polymarket)
	r.GET("/api/v1/trades/stream", handler.StreamTrades(tr, b))
	r.GET("/api/v1/trades", handler.GetTrades(tr))

	// News
	r.GET("/api/v1/news", handler.GetNews(nr))
	r.GET("/api/v1/news/stream", handler.StreamNews(nb))
	r.GET("/api/v1/news/scrapers", handler.ListScraperConfigs(sc))
	r.PATCH("/api/v1/news/scrapers/:source", handler.PatchScraperConfig(sc))
	r.DELETE("/api/v1/news/scrapers/:source/script", handler.DeleteScraperScript(sc))
	r.POST("/api/v1/news/scrapers/validate", handler.ValidateScript())

	// Polymarket markets
	r.GET("/api/v1/markets", handler.GetMarkets(pm))
	r.GET("/api/v1/markets/search", handler.SearchMarkets(pm))
	r.GET("/api/v1/events", handler.GetEvents(pm))
	r.GET("/api/v1/events/search", handler.SearchEvents(pm))

	// Polymarket watchlist
	r.GET("/api/v1/watchlist", handler.GetWatchlist(wl))
	r.PUT("/api/v1/watchlist", handler.SyncWatchlist(wl))
	r.PATCH("/api/v1/watchlist/:market_id/config", handler.UpdateWatchlistConfig(wl))

	// Options watchlist + stream
	r.GET("/api/v1/options/watchlist", handler.GetOptionsWatchlist(owl))
	r.PUT("/api/v1/options/watchlist", handler.SyncOptionsWatchlist(owl))
	r.GET("/api/v1/options/stream", handler.StreamOptions(ob))

	// Futures watchlist + stream
	r.GET("/api/v1/futures/watchlist", handler.GetFuturesWatchlist(fwl))
	r.PUT("/api/v1/futures/watchlist", handler.SyncFuturesWatchlist(fwl))
	r.GET("/api/v1/futures/stream", handler.StreamFutures(fb))

	return &Server{
		cfg:                cfg,
		broadcaster:        b,
		newsBroadcaster:    nb,
		optsBroadcaster:    ob,
		futuresBroadcaster: fb,
		server: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			Handler: r,
		},
	}
}

// Start launches Redis subscriber broadcasters then serves HTTP (or HTTPS if TLS configured).
func (s *Server) Start(ctx context.Context) error {
	go s.broadcaster.Run(ctx)
	go s.newsBroadcaster.Run(ctx)
	go s.optsBroadcaster.Run(ctx)
	go s.futuresBroadcaster.Run(ctx)
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		return s.server.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
	}
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) Addr() string {
	return s.server.Addr
}
