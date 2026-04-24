package polymarket

import (
	"fmt"
	"strconv"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/clob/ws"
)

// DetectorConfig controls what counts as an unusual trade.
type DetectorConfig struct {
	// WindowSize is the number of past trades tracked per asset for the rolling average.
	WindowSize int
	// SizeMultiplier flags a trade when its size exceeds this multiple of the rolling average.
	SizeMultiplier float64
	// MinAbsoluteSize always flags a trade when its size meets or exceeds this value,
	// regardless of the rolling average.
	MinAbsoluteSize float64
}

// UnusualTrade wraps a LastTradePriceEvent with the detection context.
type UnusualTrade struct {
	ws.LastTradePriceEvent
	RollingAvgSize float64
	Reason         string
}

// Detector tracks rolling trade size windows per asset and flags unusual trades.
type Detector struct {
	cfg     DetectorConfig
	windows map[string]*rollingWindow
}

// NewDetector creates a Detector with the given config.
func NewDetector(cfg DetectorConfig) *Detector {
	if cfg.WindowSize < 1 {
		cfg.WindowSize = 20
	}
	return &Detector{
		cfg:     cfg,
		windows: make(map[string]*rollingWindow),
	}
}

// Process updates the rolling window for the event's asset and reports whether the
// trade is unusual. It returns nil, false when the event has no parseable size.
func (d *Detector) Process(event ws.LastTradePriceEvent) (*UnusualTrade, bool) {
	size, err := strconv.ParseFloat(event.Size, 64)
	if err != nil || size <= 0 {
		return nil, false
	}

	rw, ok := d.windows[event.AssetID]
	if !ok {
		rw = newRollingWindow(d.cfg.WindowSize)
		d.windows[event.AssetID] = rw
	}

	avg := rw.avg()
	rw.add(size)

	var reason string
	switch {
	case size >= d.cfg.MinAbsoluteSize:
		reason = fmt.Sprintf("size %.2f meets absolute threshold %.2f", size, d.cfg.MinAbsoluteSize)
	case rw.full && avg > 0 && size >= d.cfg.SizeMultiplier*avg:
		reason = fmt.Sprintf("size %.2f is %.1fx the rolling avg %.2f", size, size/avg, avg)
	default:
		return nil, false
	}

	return &UnusualTrade{
		LastTradePriceEvent: event,
		RollingAvgSize:      avg,
		Reason:              reason,
	}, true
}

// IsWindowFull returns true when the rolling window for assetID has seen at least
// WindowSize trades. Returns false if no window exists yet.
func (d *Detector) IsWindowFull(assetID string) bool {
	if rw, ok := d.windows[assetID]; ok {
		return rw.full
	}
	return false
}

// RollingAvg returns the current rolling average size for the given asset,
// computed before the next trade is added. Returns 0 if no history yet.
func (d *Detector) RollingAvg(assetID string) float64 {
	if rw, ok := d.windows[assetID]; ok {
		return rw.avg()
	}
	return 0
}

// UpdateWindow adds a size value to the rolling window for the given asset.
func (d *Detector) UpdateWindow(assetID string, size float64) {
	rw, ok := d.windows[assetID]
	if !ok {
		rw = newRollingWindow(d.cfg.WindowSize)
		d.windows[assetID] = rw
	}
	rw.add(size)
}

// SetWindowSize resizes the rolling window for assetID to n.
// If n < 1, uses the detector's default WindowSize.
// No-ops if the window already has the correct size — preserving history on reconnects.
func (d *Detector) SetWindowSize(assetID string, n int) {
	if n < 1 {
		n = d.cfg.WindowSize
	}
	if existing, ok := d.windows[assetID]; ok && len(existing.sizes) == n {
		return
	}
	d.windows[assetID] = newRollingWindow(n)
}

// rollingWindow is a fixed-size circular buffer for tracking recent trade sizes.
type rollingWindow struct {
	sizes []float64
	pos   int
	full  bool
}

func newRollingWindow(size int) *rollingWindow {
	return &rollingWindow{sizes: make([]float64, size)}
}

func (rw *rollingWindow) add(v float64) {
	rw.sizes[rw.pos] = v
	rw.pos++
	if rw.pos == len(rw.sizes) {
		rw.pos = 0
		rw.full = true
	}
}

func (rw *rollingWindow) avg() float64 {
	count := len(rw.sizes)
	if !rw.full {
		count = rw.pos
	}
	if count == 0 {
		return 0
	}
	var sum float64
	for i := range count {
		sum += rw.sizes[i]
	}
	return sum / float64(count)
}
