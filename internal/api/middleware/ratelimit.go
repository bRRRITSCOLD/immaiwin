// Rate limiting middleware — protects auth endpoints from credential
// stuffing / enumeration attacks.
//
// Algorithm: fixed-window counter in Redis. Each (key, window) combo
// gets one INCR; first hit attaches an EXPIRE so the bucket evaporates
// at window end. Simple, two round-trips per request, accurate enough
// for human-vs-bot brute force (the bot sees 429 the moment it crosses
// the limit; sliding-window precision isn't necessary for /auth/login).
//
// Sliding-window upgrade path: switch to a Lua script with ZADD/ZREMRANGEBYSCORE
// over a sorted-set per key. Not done now because fixed-window's
// rejection latency is identical and the bookkeeping is ~3x more code.
//
// Failure mode: if Redis is down, the middleware fails OPEN (lets the
// request through with a warn log). Rationale: a Redis outage shouldn't
// take auth offline. Defense-in-depth comes from bcrypt cost on the
// hot path inside Login + monitoring on Redis health separately.

package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/gin-gonic/gin"
)

// RateLimitConfig captures one named rate-limit policy. Same shape used
// for /auth/login + /auth/register; different names so their counters
// are separate (a brute-force attempt on login shouldn't lock out a
// legitimate registration on the same IP).
type RateLimitConfig struct {
	// Name segments the redis key — "login" / "register" / "password_reset".
	Name string
	// Max is the request count allowed inside Window before 429 fires.
	Max int
	// Window is the bucket duration. Use minute-scale for human flows;
	// shorter windows are fine but make sure Max > expected burst from
	// a quick correction (mistyped password → 1-2 retries).
	Window time.Duration
}

// RateLimit returns gin middleware enforcing the given policy. Keys
// off c.ClientIP() (which respects the trusted-proxy chain that gin
// is configured for; in dev that's the direct remote IP).
func RateLimit(rc *rediss.Client, cfg RateLimitConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rc == nil {
			c.Next()
			return
		}
		ip := c.ClientIP()
		if ip == "" {
			ip = "unknown"
		}
		key := fmt.Sprintf("ratelimit:auth:%s:%s", cfg.Name, ip)

		count, err := rc.Incr(c.Request.Context(), key)
		if err != nil {
			// Fail open — auth must stay available even when Redis is
			// down. Log so Ops sees Redis trouble before it hides
			// other problems.
			slog.Warn("ratelimit: redis incr failed (failing open)", "key", key, "err", err)
			c.Next()
			return
		}
		// First hit in the window: attach the expiry. EXPIRE on a key
		// already past its TTL is harmless — go-redis returns false
		// without erroring, and the next first-hit will re-set.
		if count == 1 {
			if _, err := rc.Expire(c.Request.Context(), key, cfg.Window); err != nil {
				slog.Warn("ratelimit: expire failed (counter may persist)", "key", key, "err", err)
			}
		}
		if int(count) > cfg.Max {
			ttl, ttlErr := rc.TTL(c.Request.Context(), key)
			retryAfter := int(cfg.Window.Seconds())
			if ttlErr == nil && ttl > 0 {
				retryAfter = int(ttl.Seconds()) + 1
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "too many requests — slow down",
				"retry_after": retryAfter,
			})
			return
		}
		c.Next()
	}
}

