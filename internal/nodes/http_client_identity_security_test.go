package nodes

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/iplibrary"
)

func TestSecurityForwardedIdentityRequiresTrustedPeerAndWalksChain(t *testing.T) {
	resolver, err := newTrustedProxyResolver("10.0.0.0/24, 2001:db8:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, peer, xff, real, want string }{
		{"untrusted-peer", "198.51.100.8", "127.0.0.1", "127.0.0.1", "198.51.100.8"},
		{"injected-leftmost", "10.0.0.2", "127.0.0.1, 198.51.100.8, 10.0.0.3", "", "198.51.100.8"},
		{"ipv6", "2001:db8:1::2", "2001:db8:2::8, 2001:db8:1::3", "", "2001:db8:2::8"},
		{"invalid-chain", "10.0.0.2", "127.0.0.1, invalid", "127.0.0.1", "10.0.0.2"},
		{"trusted-real-header", "10.0.0.2", "", "198.51.100.8", "198.51.100.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.xff != "" {
				header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.real != "" {
				header.Set("X-Real-IP", tc.real)
			}
			if got := resolver.forwardedIP(header, tc.peer); got != tc.want {
				t.Fatalf("client = %q, want %q", got, tc.want)
			}
		})
	}
	header := http.Header{"X-Forwarded-For": {"127.0.0.1", "198.51.100.8, 10.0.0.3"}}
	if got := resolver.forwardedIP(header, "10.0.0.2"); got != "198.51.100.8" {
		t.Fatalf("multi-line chain client = %q", got)
	}
	if _, err := newTrustedProxyResolver("10.0.0.0/24,invalid"); err == nil {
		t.Fatal("invalid trusted CIDR accepted")
	}
	if none, err := newTrustedProxyResolver(""); err != nil || none.trusts("127.0.0.1") {
		t.Fatal("default trusts loopback proxy headers")
	}
}

func TestSecurityRequestIdentityPreservesDirectModeAndRejectsExtraClaims(t *testing.T) {
	previous := sharedProxyResolver
	defer func() { sharedProxyResolver = previous }()
	var err error
	sharedProxyResolver, err = newTrustedProxyResolver("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, peer, mode, value, want string }{
		{"direct", "10.0.0.2:1234", serverconfigs.HTTPRemoteAddrTypeDefault, "${rawRemoteAddr}", "10.0.0.2"},
		{"proxy", "10.0.0.2:1234", serverconfigs.HTTPRemoteAddrTypeProxy, "${remoteAddr}", "198.51.100.8"},
		{"untrusted-custom", "198.51.100.9:1234", serverconfigs.HTTPRemoteAddrTypeRequestHeader, "${header.X-Real-IP}", "198.51.100.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := &serverconfigs.HTTPRemoteAddrConfig{IsOn: true, Type: tc.mode, Value: tc.value}
			if err := config.Init(); err != nil {
				t.Fatal(err)
			}
			req := &HTTPRequest{
				RawReq: &http.Request{RemoteAddr: tc.peer, Header: http.Header{"X-Forwarded-For": {"127.0.0.1, 198.51.100.8"}, "X-Real-Ip": {"127.0.0.1"}}},
				web:    &serverconfigs.HTTPWebConfig{RemoteAddr: config},
			}
			if got := req.requestRemoteAddr(true); got != tc.want {
				t.Fatalf("client = %q, want %q", got, tc.want)
			}
			if got := req.requestRemoteAddrs(); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Fatalf("extra unverified identities = %#v", got)
			}
		})
	}
}

func TestSecurityProxyLoopbackClaimHasNoInternalWhitelistExemption(t *testing.T) {
	allowed, white, _ := iplibrary.AllowClientIP("127.0.0.1", 0)
	if !allowed || white {
		t.Fatalf("proxy-reported loopback inherited internal exemption: allowed=%t white=%t", allowed, white)
	}
}
