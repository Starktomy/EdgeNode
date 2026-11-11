package nodes

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

func TestSecurityOriginTLSAuthenticatesPrivateCAAndHostname(t *testing.T) {
	t.Setenv("EDGE_ALLOW_INSECURE_ORIGIN_TLS", "")
	t.Setenv("EDGE_ORIGIN_CA_FILE", "")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "trusted") }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	origin := &serverconfigs.OriginConfig{Addr: &serverconfigs.NetworkAddressConfig{Host: u.Hostname(), PortRange: u.Port()}}
	config, err := originTLSConfig(origin, u.Hostname())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	if resp, err := (&http.Client{Transport: transport}).Get(server.URL); err == nil {
		resp.Body.Close()
		t.Fatal("untrusted origin certificate accepted")
	}
	ca := filepath.Join(t.TempDir(), "origin-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDGE_ORIGIN_CA_FILE", ca)
	config, err = originTLSConfig(origin, u.Hostname())
	if err != nil {
		t.Fatal(err)
	}
	trusted := &http.Transport{TLSClientConfig: config}
	defer trusted.CloseIdleConnections()
	resp, err := (&http.Client{Transport: trusted}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	badHost := config.Clone()
	badHost.ServerName = "attacker.test"
	badTransport := &http.Transport{TLSClientConfig: badHost}
	defer badTransport.CloseIdleConnections()
	if resp, err := (&http.Client{Transport: badTransport}).Get(server.URL); err == nil {
		resp.Body.Close()
		t.Fatal("origin hostname mismatch accepted")
	}
}

func TestSecurityOriginRetriesRequireSafeBodylessMethod(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "CONNECT"} {
		if canRetryOriginRequest(httptest.NewRequest(method, "https://example.test/", nil)) {
			t.Fatalf("mutation %s can be repeated after ambiguous origin failure", method)
		}
	}
	if !canRetryOriginRequest(httptest.NewRequest("GET", "https://example.test/", nil)) {
		t.Fatal("safe GET failover was disabled")
	}
	if canRetryOriginRequest(httptest.NewRequest("GET", "https://example.test/", strings.NewReader("body"))) {
		t.Fatal("live body can be replayed")
	}
}

func TestSecurityNodeCandidateRejectsInvalidTimezoneBeforePublication(t *testing.T) {
	if err := validateNodeCandidate(&nodeconfigs.NodeConfig{TimeZone: "missing/timezone"}); err == nil {
		t.Fatal("invalid timezone can overwrite the last known good configuration")
	}
}
