package nodes

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/iplibrary"
)

const trustedProxyCIDRsEnv = "EDGE_TRUSTED_PROXY_CIDRS"
const maxForwardedAddresses = 64

type trustedProxyResolver struct{ prefixes []netip.Prefix }

var sharedProxyResolver = loadTrustedProxyResolver()

func loadTrustedProxyResolver() *trustedProxyResolver {
	resolver, err := newTrustedProxyResolver(os.Getenv(trustedProxyCIDRsEnv))
	if err != nil {
		log.Printf("[%s] invalid proxy configuration; forwarded headers are disabled: %v", trustedProxyCIDRsEnv, err)
		return &trustedProxyResolver{}
	}
	return resolver
}

func newTrustedProxyResolver(cidrs string) (*trustedProxyResolver, error) {
	resolver := &trustedProxyResolver{}
	if strings.TrimSpace(cidrs) == "" {
		return resolver, nil
	}
	for _, raw := range strings.Split(cidrs, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("expected comma-separated proxy CIDRs: %w", err)
		}
		resolver.prefixes = append(resolver.prefixes, prefix.Masked())
	}
	return resolver, nil
}

func socketPeerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func (resolver *trustedProxyResolver) trusts(peer string) bool {
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range resolver.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parsedClientIP(value string) (string, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
		return "", false
	}
	return addr.Unmap().String(), true
}

func (resolver *trustedProxyResolver) forwardedIP(header http.Header, peer string) string {
	if !resolver.trusts(peer) {
		return peer
	}
	values := header.Values("X-Forwarded-For")
	if len(values) > 0 {
		var chain []string
		var size int
		for _, value := range values {
			size += len(value)
			if size > 4096 {
				return peer
			}
			for _, item := range strings.Split(value, ",") {
				ip, ok := parsedClientIP(item)
				if !ok || len(chain) == maxForwardedAddresses {
					return peer
				}
				chain = append(chain, ip)
			}
		}
		client := peer
		for index := len(chain) - 1; index >= 0; index-- {
			if !resolver.trusts(client) {
				return client
			}
			client = chain[index]
		}
		return client
	}
	values = header.Values("X-Real-IP")
	if len(values) == 1 {
		if client, ok := parsedClientIP(values[0]); ok {
			return client
		}
	}
	return peer
}

func (this *HTTPRequest) usesTrustedProxyIdentity() bool {
	if this.RawReq == nil || !sharedProxyResolver.trusts(socketPeerIP(this.RawReq.RemoteAddr)) {
		return false
	}
	if this.web != nil && this.web.RemoteAddr != nil && this.web.RemoteAddr.IsOn {
		config := this.web.RemoteAddr
		if config.Type == serverconfigs.HTTPRemoteAddrTypeDefault || config.Value == "${rawRemoteAddr}" {
			return false
		}
	}
	return true
}

func (this *HTTPRequest) allowClientIP(ip string) (bool, bool, int64) {
	if this.usesTrustedProxyIdentity() {
		return iplibrary.AllowClientIP(ip, this.ReqServer.Id)
	}
	return iplibrary.AllowIP(ip, this.ReqServer.Id)
}
