package workflow

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// BuildWSDialer reads a websocket Connection's encrypted config and
// returns a Dialer + the full request URL + request headers ready to
// hand to `Dialer.Dial`. Provider-agnostic: every WebSocket source we
// support (Polygon, Binance, OpenAI Realtime, etc.) accepts a static
// user-supplied bearer/header/query token, so we layer those onto a
// vanilla gorilla dial.
//
// Connection.Config keys:
//
//	url                       string  required, `wss://...` or `ws://...`
//	auth_header               string  optional, e.g. `Authorization: Bearer <token>`
//	auth_query                string  optional, e.g. `token=...&apiKey=...`
//	headers                   string  optional, newline-separated `Key: Value` pairs
//	tls_insecure_skip_verify  string  optional ("true" / "false"); dev only
//	handshake_timeout_seconds string  optional, default 10
func BuildWSDialer(cfg map[string]string) (*websocket.Dialer, string, http.Header, error) {
	raw := strings.TrimSpace(cfg["url"])
	if raw == "" {
		return nil, "", nil, errors.New("websocket connection: url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", nil, fmt.Errorf("websocket connection: parse url: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, "", nil, fmt.Errorf("websocket connection: url must use ws:// or wss:// (got %q)", u.Scheme)
	}

	if q := strings.TrimSpace(cfg["auth_query"]); q != "" {
		extra, perr := url.ParseQuery(strings.TrimPrefix(q, "?"))
		if perr != nil {
			return nil, "", nil, fmt.Errorf("websocket connection: parse auth_query: %w", perr)
		}
		merged := u.Query()
		for k, vs := range extra {
			for _, v := range vs {
				merged.Add(k, v)
			}
		}
		u.RawQuery = merged.Encode()
	}

	hdr := http.Header{}
	if h := strings.TrimSpace(cfg["auth_header"]); h != "" {
		k, v, ok := splitHeaderLine(h)
		if !ok {
			return nil, "", nil, fmt.Errorf("websocket connection: auth_header must be \"Key: Value\"")
		}
		hdr.Set(k, v)
	}
	if blob := strings.TrimSpace(cfg["headers"]); blob != "" {
		for _, line := range strings.Split(blob, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			k, v, ok := splitHeaderLine(line)
			if !ok {
				return nil, "", nil, fmt.Errorf("websocket connection: header line %q must be \"Key: Value\"", line)
			}
			hdr.Set(k, v)
		}
	}

	handshake := 10 * time.Second
	if s := strings.TrimSpace(cfg["handshake_timeout_seconds"]); s != "" {
		if n, perr := time.ParseDuration(s + "s"); perr == nil && n > 0 {
			handshake = n
		}
	}

	insecure := strings.EqualFold(strings.TrimSpace(cfg["tls_insecure_skip_verify"]), "true")
	dialer := &websocket.Dialer{
		HandshakeTimeout: handshake,
		Proxy:            http.ProxyFromEnvironment,
	}
	if insecure {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // dev-only opt-in
	}
	return dialer, u.String(), hdr, nil
}

func splitHeaderLine(s string) (string, string, bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(s[:i])
	v := strings.TrimSpace(s[i+1:])
	if k == "" {
		return "", "", false
	}
	return k, v, true
}
