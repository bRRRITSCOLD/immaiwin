package futures

import "time"

// Trade is a single futures trade print from the streamer.
type Trade struct {
	Symbol    string    // active contract, e.g. "/CLM25"
	Root      string    // root symbol, e.g. "/CL"
	Price     float64
	Size      int64
	Volume    int64     // cumulative session volume
	OI        int64     // open interest
	Timestamp time.Time
}

// WatchlistItem is a futures root symbol being watched.
type WatchlistItem struct {
	Symbol  string    `bson:"symbol"   json:"symbol"`
	AddedAt time.Time `bson:"added_at" json:"added_at"`
}
