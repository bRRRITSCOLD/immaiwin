package workflow

import (
	"net"
	"testing"

	"github.com/stretchr/testify/suite"
)

type SSRFGuardSuite struct {
	suite.Suite
}

func (s *SSRFGuardSuite) SetupSuite()    {}
func (s *SSRFGuardSuite) TearDownSuite() {}
func (s *SSRFGuardSuite) SetupTest()     {}
func (s *SSRFGuardSuite) TearDownTest()  {}

// TestSSRFGuardSuite is the test entrypoint for the http_request
// SSRF dial-guard helpers.
func TestSSRFGuardSuite(t *testing.T) {
	suite.Run(t, new(SSRFGuardSuite))
}

// TestIsPrivateOrReservedIP_AllReservedRanges_AreRefused verifies the
// deny-list covers every range that has no legitimate reason to be
// dialed from a tenant workflow: cloud metadata (169.254.169.254),
// loopback, link-local, RFC1918, CGNAT, multicast, unspecified, IPv6
// ULA + link-local + loopback + multicast.
func (s *SSRFGuardSuite) TestIsPrivateOrReservedIP_AllReservedRanges_AreRefused() {
	cases := []string{
		"127.0.0.1",
		"127.0.0.53",
		"0.0.0.0",
		"0.1.2.3",
		"169.254.0.1",
		"169.254.169.254",
		"10.0.0.1",
		"10.255.255.255",
		"172.16.0.1",
		"172.31.255.255",
		"192.168.1.1",
		"100.64.0.1",
		"100.127.255.255",
		"224.0.0.1",
		"::1",
		"::",
		"fe80::1",
		"fc00::1",
		"fdff::1",
		"ff02::1",
	}
	for _, ipStr := range cases {
		ip := net.ParseIP(ipStr)
		s.Require().NotNil(ip, "ParseIP(%q)", ipStr)
		s.True(isPrivateOrReservedIP(ip), "%s must be refused", ipStr)
	}
}

// TestIsPrivateOrReservedIP_PublicIPs_AreAllowed verifies the allow
// path: routable IPs (well-known DNS, etc.) must NOT be refused.
func (s *SSRFGuardSuite) TestIsPrivateOrReservedIP_PublicIPs_AreAllowed() {
	cases := []string{
		"8.8.8.8",
		"1.1.1.1",
		"172.32.0.1",
		"192.169.0.1",
		"100.128.0.1",
		"2001:4860:4860::8888",
	}
	for _, ipStr := range cases {
		ip := net.ParseIP(ipStr)
		s.Require().NotNil(ip, "ParseIP(%q)", ipStr)
		s.False(isPrivateOrReservedIP(ip), "%s must be allowed", ipStr)
	}
}

// TestSsrfDialControl_AllowedHost_ReturnsNoGuard verifies the
// allow-list bypass: when the URL hostname is on the allowed_hosts
// list the control returns nil (no Dial restriction at all).
func (s *SSRFGuardSuite) TestSsrfDialControl_AllowedHost_ReturnsNoGuard() {
	s.Nil(ssrfDialControl([]string{"internal.svc", "api.example.com"}, "api.example.com"),
		"host on allow-list must yield nil control")
	s.Nil(ssrfDialControl([]string{"  Api.Example.COM  "}, "api.example.com"),
		"match must be case-insensitive + whitespace-tolerant")
}

// TestSsrfDialControl_PrivateIP_Refused verifies the deny path:
// without an allow-list entry, dialing a private IP returns an error
// whose message names the IP and points the operator at allowed_hosts.
func (s *SSRFGuardSuite) TestSsrfDialControl_PrivateIP_Refused() {
	ctrl := ssrfDialControl(nil, "internal.svc")
	s.Require().NotNil(ctrl)
	err := ctrl("tcp", "169.254.169.254:80", nil)
	s.Require().Error(err)
	s.Contains(err.Error(), "169.254.169.254", "error must name the refused IP")
	s.Contains(err.Error(), "allowed_hosts", "error must hint at the allowed_hosts opt-in")
}

// TestSsrfDialControl_PublicIP_Allowed verifies the deny-then-allow
// path: a public IP passes the control even without an allow-list.
func (s *SSRFGuardSuite) TestSsrfDialControl_PublicIP_Allowed() {
	ctrl := ssrfDialControl(nil, "example.com")
	s.Require().NotNil(ctrl)
	s.NoError(ctrl("tcp", "8.8.8.8:443", nil), "public IP must pass without allow-list")
}
