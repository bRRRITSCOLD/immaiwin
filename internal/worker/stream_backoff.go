package worker

import (
	"errors"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	backoffBase = 15 * time.Second
	backoffMax  = 30 * time.Minute
)

// StreamBackoff tracks consecutive stream closes and returns exponentially
// increasing backoff durations. Reset when live data is received.
type StreamBackoff struct {
	consecutive int
}

// Next increments the consecutive counter and returns the backoff duration.
// Base is 15s; doubles each attempt; caps at 30 min.
// Policy-violation close → immediate max.
// Close 1000 with named session text → skip exponential and go straight to max.
func (sb *StreamBackoff) Next(err error) time.Duration {
	sb.consecutive++

	if err != nil {
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			switch ce.Code {
			case websocket.ClosePolicyViolation: // 1008 — auth/policy
				return backoffMax
			case websocket.CloseNormalClosure: // 1000
				txt := strings.ToLower(ce.Text)
				if strings.Contains(txt, "market") || strings.Contains(txt, "session") ||
					strings.Contains(txt, "end") {
					return backoffMax
				}
			}
		}
	}

	shift := sb.consecutive - 1
	if shift > 20 {
		shift = 20
	}
	bd := backoffBase * (1 << shift)
	if bd > backoffMax {
		bd = backoffMax
	}
	return bd
}

// Reset clears the consecutive counter. Call when a live message is received.
func (sb *StreamBackoff) Reset() {
	sb.consecutive = 0
}
