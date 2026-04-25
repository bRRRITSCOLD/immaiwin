package options

import "context"

// Provider is the abstraction over any options data source (Tradier, Polygon, etc.).
// The watcher worker depends only on this interface — swap impl without changing worker logic.
type Provider interface {
	// GetExpirations returns available expiration date strings (YYYY-MM-DD) for a symbol.
	GetExpirations(ctx context.Context, symbol string) ([]string, error)

	// GetChain returns all contracts for a symbol on a given expiration date (YYYY-MM-DD).
	GetChain(ctx context.Context, symbol, expiration string) ([]Contract, error)

	// StreamTrades opens a real-time trade stream for the given option symbols (OCC format).
	// The returned channel is closed when ctx is cancelled or an unrecoverable error occurs.
	StreamTrades(ctx context.Context, optionSymbols []string) (<-chan Trade, error)

	Close() error
}
