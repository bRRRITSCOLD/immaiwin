package options

import "time"

// Contract is a single options contract with market data.
type Contract struct {
	Symbol     string    // OCC symbol e.g. "AAPL210416C00125000"
	Underlying string    // e.g. "AAPL"
	Strike     float64
	Expiration time.Time
	Type       string  // "call" / "put"
	Bid        float64
	Ask        float64
	Last       float64
	Volume     int64
	OI         int64   // open interest
	IV         float64 // implied volatility (0–1)
}

// Trade is a single executed options trade (print).
type Trade struct {
	Symbol     string // OCC option symbol
	Underlying string
	Strike     float64
	Expiration time.Time
	Type       string // "call" / "put"
	Price      float64
	Size       int64 // contracts (notional = Size * 100 * Price)
	Exchange   string
	Timestamp  time.Time
}

// WatchlistItem is an underlying ticker being watched for unusual options activity.
type WatchlistItem struct {
	Symbol  string    `bson:"symbol"   json:"symbol"`
	AddedAt time.Time `bson:"added_at" json:"added_at"`
}

// DetectionResult wraps a trade with unusual-activity annotation.
type DetectionResult struct {
	Trade         Trade
	Unusual       bool
	Reason        string
	VolumeOIRatio float64
}
