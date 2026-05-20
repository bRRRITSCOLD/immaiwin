package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"
)

type WSDialerSuite struct {
	suite.Suite
}

func (s *WSDialerSuite) SetupSuite()    {}
func (s *WSDialerSuite) TearDownSuite() {}
func (s *WSDialerSuite) SetupTest()     {}
func (s *WSDialerSuite) TearDownTest()  {}

// TestWSDialerSuite is the test entrypoint for BuildWSDialer.
func TestWSDialerSuite(t *testing.T) {
	suite.Run(t, new(WSDialerSuite))
}

// TestBuildWSDialer_MissingURL_ReturnsError verifies that an empty
// url surfaces a config-level error before any network IO.
func (s *WSDialerSuite) TestBuildWSDialer_MissingURL_ReturnsError() {
	_, _, _, err := BuildWSDialer(map[string]string{})
	s.Require().Error(err)
	s.Contains(err.Error(), "url is required")
}

// TestBuildWSDialer_NonWSScheme_ReturnsError verifies the dialer
// refuses http:// / https:// / ftp:// schemes — must be ws:// or wss://.
func (s *WSDialerSuite) TestBuildWSDialer_NonWSScheme_ReturnsError() {
	for _, raw := range []string{"http://example.com", "https://example.com", "ftp://example.com"} {
		_, _, _, err := BuildWSDialer(map[string]string{"url": raw})
		s.Require().Errorf(err, "should reject %q", raw)
		s.Contains(err.Error(), "ws://")
	}
}

// TestBuildWSDialer_AuthQuery_MergedIntoURL verifies that auth_query
// values are appended to the dial URL's existing query (no overwrite,
// no duplication of the existing keys).
func (s *WSDialerSuite) TestBuildWSDialer_AuthQuery_MergedIntoURL() {
	_, target, _, err := BuildWSDialer(map[string]string{
		"url":        "wss://stream.example.com/feed?x=1",
		"auth_query": "token=abc&apiKey=xyz",
	})
	s.Require().NoError(err)
	s.Contains(target, "x=1")
	s.Contains(target, "token=abc")
	s.Contains(target, "apiKey=xyz")
}

// TestBuildWSDialer_AuthHeader_SetOnRequest verifies that the
// auth_header line ("Authorization: Bearer X") is parsed into the
// returned http.Header so the dialer sends it on handshake.
func (s *WSDialerSuite) TestBuildWSDialer_AuthHeader_SetOnRequest() {
	_, _, hdr, err := BuildWSDialer(map[string]string{
		"url":         "wss://stream.example.com",
		"auth_header": "Authorization: Bearer abc.def.ghi",
	})
	s.Require().NoError(err)
	s.Equal("Bearer abc.def.ghi", hdr.Get("Authorization"))
}

// TestBuildWSDialer_AuthHeader_Malformed_ReturnsError verifies that
// a missing colon in auth_header surfaces a config-level error rather
// than silently dropping the header.
func (s *WSDialerSuite) TestBuildWSDialer_AuthHeader_Malformed_ReturnsError() {
	_, _, _, err := BuildWSDialer(map[string]string{
		"url":         "wss://stream.example.com",
		"auth_header": "BearerNoColon",
	})
	s.Require().Error(err)
	s.Contains(err.Error(), "Key: Value")
}

// TestBuildWSDialer_ExtraHeaders_NewlineSeparated verifies that the
// headers blob is split on newlines and every "Key: Value" line lands
// on the outgoing request header.
func (s *WSDialerSuite) TestBuildWSDialer_ExtraHeaders_NewlineSeparated() {
	_, _, hdr, err := BuildWSDialer(map[string]string{
		"url":     "wss://stream.example.com",
		"headers": "X-Tenant: acme\nX-Trace: abc123",
	})
	s.Require().NoError(err)
	s.Equal("acme", hdr.Get("X-Tenant"))
	s.Equal("abc123", hdr.Get("X-Trace"))
}

// TestBuildWSDialer_InsecureTLS_OptIn verifies that the dialer enables
// InsecureSkipVerify only when tls_insecure_skip_verify=="true" (dev
// escape hatch). Default keeps verification on.
func (s *WSDialerSuite) TestBuildWSDialer_InsecureTLS_OptIn() {
	d, _, _, err := BuildWSDialer(map[string]string{
		"url":                      "wss://self-signed.example.com",
		"tls_insecure_skip_verify": "true",
	})
	s.Require().NoError(err)
	s.Require().NotNil(d.TLSClientConfig)
	s.True(d.TLSClientConfig.InsecureSkipVerify)

	d2, _, _, err := BuildWSDialer(map[string]string{"url": "wss://stream.example.com"})
	s.Require().NoError(err)
	s.Nil(d2.TLSClientConfig)
}

// TestBuildWSDialer_HandshakeTimeout_Override verifies that
// handshake_timeout_seconds parses + applies; absent value falls back
// to the 10s default.
func (s *WSDialerSuite) TestBuildWSDialer_HandshakeTimeout_Override() {
	d, _, _, err := BuildWSDialer(map[string]string{
		"url":                       "wss://stream.example.com",
		"handshake_timeout_seconds": "3",
	})
	s.Require().NoError(err)
	s.Equal(3, int(d.HandshakeTimeout.Seconds()))

	d2, _, _, err := BuildWSDialer(map[string]string{"url": "wss://stream.example.com"})
	s.Require().NoError(err)
	s.Equal(10, int(d2.HandshakeTimeout.Seconds()))
}

// TestBuildWSDialer_AuthQuery_LeadingQuestionMark_Stripped verifies a
// common UX foot-gun: pasting `?token=…` instead of `token=…`. The
// dialer strips the leading `?` before parsing so the resulting URL is
// still a single well-formed query string.
func (s *WSDialerSuite) TestBuildWSDialer_AuthQuery_LeadingQuestionMark_Stripped() {
	_, target, _, err := BuildWSDialer(map[string]string{
		"url":        "wss://stream.example.com",
		"auth_query": "?token=abc",
	})
	s.Require().NoError(err)
	s.Contains(target, "token=abc")
	s.False(strings.Contains(target, "??"))
}
