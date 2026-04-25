package watchlist

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// DefaultWindowSize is the default rolling-average window (number of trades).
	DefaultWindowSize = 20
	// DefaultExpr is the expression equivalent of the built-in detector defaults.
	// Size >= 10000 catches absolute-size trades; the second clause mirrors the
	// rolling-average multiplier (3x) but only once the window is full.
	DefaultExpr = `Size >= 10000 || (WindowFull && Avg > 0 && Size >= Avg * 3.0)`
)

type WatchlistItem struct {
	ID          bson.ObjectID `bson:"_id,omitempty"          json:"id"`
	MarketID    string        `bson:"market_id"              json:"market_id"`
	Question    string        `bson:"question"               json:"question"`
	Slug        string        `bson:"slug"                   json:"slug"`
	UnusualExpr string        `bson:"unusual_expr,omitempty" json:"unusual_expr,omitempty"`
	WindowSize  int           `bson:"window_size,omitempty"  json:"window_size,omitempty"`
	AddedAt     time.Time     `bson:"added_at"               json:"added_at"`
}
