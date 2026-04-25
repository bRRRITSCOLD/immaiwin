package options

import "time"

// OptionsEvent is published to Redis for each processed options trade.
// All trades are published (Unusual=false for normal prints, true for flagged).
type OptionsEvent struct {
	Symbol        string    `json:"symbol"`
	Underlying    string    `json:"underlying"`
	Strike        float64   `json:"strike"`
	Expiration    time.Time `json:"expiration"`
	Type          string    `json:"type"` // "call" / "put"
	Price         float64   `json:"price"`
	Size          int64     `json:"size"`
	Unusual       bool      `json:"unusual"`
	Reason        string    `json:"reason,omitempty"`
	VolumeOIRatio float64   `json:"volume_oi_ratio,omitempty"`
	DetectedAt    time.Time `json:"detected_at"`
}
