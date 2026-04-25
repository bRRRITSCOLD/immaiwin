package options

import (
	"fmt"
	"sync"
)

const (
	DefaultBlockThreshold  = 50   // contracts — single print >= this is a block
	DefaultVolumeOIRatio   = 3.0  // vol/OI ratio threshold for unusual activity
	DefaultMinOI           = 10   // ignore contracts with OI below this (avoids noisy OTM)
)

// Detector tracks cumulative volume per option symbol within a session
// and applies configurable rules to flag unusual prints.
type Detector struct {
	mu              sync.Mutex
	cumVolume       map[string]int64 // OCC symbol → cumulative session volume
	blockThreshold  int64
	volumeOIRatio   float64
	minOI           int64
}

func NewDetector() *Detector {
	return &Detector{
		cumVolume:      make(map[string]int64),
		blockThreshold: DefaultBlockThreshold,
		volumeOIRatio:  DefaultVolumeOIRatio,
		minOI:          DefaultMinOI,
	}
}

// Process evaluates a trade print against detection rules.
// oi is the open interest at time of evaluation (from chain snapshot).
func (d *Detector) Process(t Trade, oi int64) DetectionResult {
	d.mu.Lock()
	d.cumVolume[t.Symbol] += t.Size
	cumVol := d.cumVolume[t.Symbol]
	d.mu.Unlock()

	result := DetectionResult{Trade: t}

	var reasons []string

	// Rule 1: block trade
	if t.Size >= d.blockThreshold {
		reasons = append(reasons, fmt.Sprintf("block %d contracts", t.Size))
	}

	// Rule 2: vol/OI ratio spike
	if oi >= d.minOI {
		ratio := float64(cumVol) / float64(oi)
		result.VolumeOIRatio = ratio
		if ratio >= d.volumeOIRatio {
			reasons = append(reasons, fmt.Sprintf("vol/OI %.1fx", ratio))
		}
	}

	if len(reasons) > 0 {
		result.Unusual = true
		result.Reason = buildReason(t, reasons)
	}

	return result
}

// ResetVolumes clears cumulative volume tracking (call at session open each day).
func (d *Detector) ResetVolumes() {
	d.mu.Lock()
	d.cumVolume = make(map[string]int64)
	d.mu.Unlock()
}

func buildReason(t Trade, reasons []string) string {
	typeStr := t.Type
	s := fmt.Sprintf("%s $%.2f %s exp %s — ", t.Underlying, t.Strike, typeStr, t.Expiration.Format("2006-01-02"))
	for i, r := range reasons {
		if i > 0 {
			s += ", "
		}
		s += r
	}
	return s
}
