package watchlist

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type WatchlistItem struct {
	ID       bson.ObjectID `bson:"_id,omitempty"   json:"id"`
	MarketID string        `bson:"market_id"       json:"market_id"`
	Question string        `bson:"question"        json:"question"`
	Slug     string        `bson:"slug"            json:"slug"`
	AddedAt  time.Time     `bson:"added_at"        json:"added_at"`
}
