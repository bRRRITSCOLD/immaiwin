package trade

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type TradeEvent struct {
	AssetID        string    `json:"asset_id"`
	Market         string    `json:"market"`
	MarketQuestion string    `json:"market_question,omitempty"`
	TokenOutcome   string    `json:"token_outcome,omitempty"`
	Price          string    `json:"price"`
	Size           string    `json:"size"`
	Side           string    `json:"side"`
	FeeRateBps     string    `json:"fee_rate_bps"`
	Timestamp      string    `json:"timestamp"`
	Unusual        bool      `json:"unusual"`
	RollingAvgSize float64   `json:"rolling_avg_size,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	DetectedAt     time.Time `json:"detected_at"`
}

type Trade struct {
	ID             bson.ObjectID `bson:"_id,omitempty"     json:"id"`
	AssetID        string        `bson:"asset_id"          json:"asset_id"`
	Market         string        `bson:"market"            json:"market"`
	MarketQuestion string        `bson:"market_question"   json:"market_question"`
	TokenOutcome   string        `bson:"token_outcome"     json:"token_outcome"`
	Price          string        `bson:"price"             json:"price"`
	Size           string        `bson:"size"              json:"size"`
	Side           string        `bson:"side"              json:"side"`
	FeeRateBps     string        `bson:"fee_rate_bps"      json:"fee_rate_bps"`
	Timestamp      string        `bson:"timestamp"         json:"timestamp"`
	RollingAvgSize float64       `bson:"rolling_avg_size"  json:"rolling_avg_size"`
	Reason         string        `bson:"reason"            json:"reason"`
	DetectedAt     time.Time     `bson:"detected_at"       json:"detected_at"`
}
