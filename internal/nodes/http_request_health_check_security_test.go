package nodes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/healthcheck"
	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

func TestSecurityHealthCheckRequiresSignedRequestAndRejectsReplay(t *testing.T) {
	config := &nodeconfigs.NodeConfig{NodeId: "health-node-751", Secret: "test-health-secret"}
	newProbe := func() *HTTPRequest {
		raw := httptest.NewRequest(http.MethodGet, "http://edge.example.test/check", nil)
		raw.RemoteAddr = "198.51.100.8:1234"
		req := &HTTPRequest{RawReq: raw, RawWriter: httptest.NewRecorder(), ReqServer: &serverconfigs.ServerConfig{}, nodeConfig: config}
		req.init()
		return req
	}
	req := newProbe()
	legacy := "eyJvbmx5QmFzaWNSZXF1ZXN0Ijp0cnVlLCJhY2Nlc3NMb2dJc09uIjpmYWxzZX0="
	req.RawReq.Header.Set(serverconfigs.HealthCheckHeaderName, legacy)
	if stopped := req.doHealthCheck(legacy, &req.isHealthCheck); stopped || req.isHealthCheck || req.disableLog {
		t.Fatal("legacy header acquired probe privileges")
	}
	if req.RawReq.Header.Get(serverconfigs.HealthCheckHeaderName) != "" {
		t.Fatal("untrusted probe header forwarded")
	}

	req = newProbe()
	token, err := healthcheck.Sign(config.NodeId, config.Secret, req.RawReq, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := req.doHealthCheck(token, &req.isHealthCheck); !stopped || !req.isHealthCheck || !req.disableLog {
		t.Fatal("signed probe was not accepted")
	}
	if got := req.RawWriter.(*httptest.ResponseRecorder).Code; got != http.StatusOK {
		t.Fatalf("probe status = %d", got)
	}
	replay := newProbe()
	if stopped := replay.doHealthCheck(token, &replay.isHealthCheck); stopped || replay.isHealthCheck || replay.disableLog {
		t.Fatal("replayed probe acquired privileges")
	}
}
