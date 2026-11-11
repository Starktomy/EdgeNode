package nodes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/firewallconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/waf"
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
)

func securityWAFRequest(t *testing.T, entity string) *HTTPRequest {
	t.Helper()
	raw := httptest.NewRequest(http.MethodPost, "http://example.test/", strings.NewReader(entity))
	raw.ContentLength = -1
	raw.RemoteAddr = "198.51.100.8:1234"
	req := &HTTPRequest{RawReq: raw, RawWriter: httptest.NewRecorder(), ReqServer: &serverconfigs.ServerConfig{}}
	req.init()
	req.web.FirewallRef = &firewallconfigs.HTTPFirewallRef{IsOn: true}
	return req
}

func securityWAFPolicy(param string, outbound bool) *firewallconfigs.HTTPFirewallPolicy {
	group := &firewallconfigs.HTTPFirewallRuleGroup{
		Id: 1, IsOn: true,
		Sets: []*firewallconfigs.HTTPFirewallRuleSet{{
			Id: 1, IsOn: true, Connector: waf.RuleConnectorAnd,
			Rules: []*firewallconfigs.HTTPFirewallRule{{Id: 1, Param: param, Operator: waf.RuleOperatorContains, Value: "blocked-marker"}},
		}},
	}
	policy := &firewallconfigs.HTTPFirewallPolicy{Id: 751, IsOn: true, Mode: firewallconfigs.FirewallModeDefend}
	if outbound {
		policy.Outbound = &firewallconfigs.HTTPFirewallOutboundConfig{IsOn: true, Groups: []*firewallconfigs.HTTPFirewallRuleGroup{group}}
	} else {
		policy.Inbound = &firewallconfigs.HTTPFirewallInboundConfig{IsOn: true, Groups: []*firewallconfigs.HTTPFirewallRuleGroup{group}}
	}
	return policy
}

func TestSecurityWAFInspectionFailuresReturnExplicitStatuses(t *testing.T) {
	previous := waf.SharedWAFManager
	defer func() { waf.SharedWAFManager = previous }()
	waf.SharedWAFManager = waf.NewWAFManager()
	policy := securityWAFPolicy("${requestBody}", false)
	if err := waf.SharedWAFManager.UpdatePolicies([]*firewallconfigs.HTTPFirewallPolicy{policy}); err != nil {
		t.Fatal(err)
	}
	req := securityWAFRequest(t, strings.Repeat("x", 33))
	req.web.FirewallPolicy = &firewallconfigs.HTTPFirewallPolicy{MaxRequestBodySize: 32}
	if blocked, _ := req.checkWAFRequest(policy, false, false, false, false); !blocked {
		t.Fatal("oversized inspected body allowed")
	}
	if got := req.RawWriter.(*httptest.ResponseRecorder).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("request status = %d", got)
	}

	policy = securityWAFPolicy("${requestJSON.name}", false)
	if err := waf.SharedWAFManager.UpdatePolicies([]*firewallconfigs.HTTPFirewallPolicy{policy}); err != nil {
		t.Fatal(err)
	}
	req = securityWAFRequest(t, `{"name":`)
	if blocked, _ := req.checkWAFRequest(policy, false, false, false, false); !blocked {
		t.Fatal("unparseable inspected body allowed")
	}
	if got := req.RawWriter.(*httptest.ResponseRecorder).Code; got != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d", got)
	}

	policy = securityWAFPolicy("${responseBody}", true)
	if err := waf.SharedWAFManager.UpdatePolicies([]*firewallconfigs.HTTPFirewallPolicy{policy}); err != nil {
		t.Fatal(err)
	}
	req = securityWAFRequest(t, "")
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("unused")), Header: http.Header{}, ContentLength: requests.MaxInspectionBodyBytes + 1}
	if blocked, _ := req.checkWAFResponse(policy, resp, false, false, false); !blocked {
		t.Fatal("uninspected oversized response allowed")
	}
	if got := req.RawWriter.(*httptest.ResponseRecorder).Code; got != http.StatusBadGateway {
		t.Fatalf("response status = %d", got)
	}
}
