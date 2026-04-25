package futures

import "time"

// FuturesEvent is published to Redis for each processed futures trade.
type FuturesEvent struct {
	Symbol     string    `json:"symbol"`
	Root       string    `json:"root"`
	Price      float64   `json:"price"`
	Size       int64     `json:"size"`
	Volume     int64     `json:"volume"`
	OI         int64     `json:"oi"`
	Unusual    bool      `json:"unusual"`
	Reason     string    `json:"reason,omitempty"`
	DetectedAt time.Time `json:"detected_at"`
}
