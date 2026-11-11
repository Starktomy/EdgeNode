package waf

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/firewallconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
)

func securityTestPolicy(id int64, expression string) *firewallconfigs.HTTPFirewallPolicy {
	return &firewallconfigs.HTTPFirewallPolicy{
		Id: id, IsOn: true,
		Inbound: &firewallconfigs.HTTPFirewallInboundConfig{
			IsOn: true,
			Groups: []*firewallconfigs.HTTPFirewallRuleGroup{{
				Id: 1, IsOn: true,
				Sets: []*firewallconfigs.HTTPFirewallRuleSet{{
					Id: 1, IsOn: true, Connector: RuleConnectorAnd,
					Rules: []*firewallconfigs.HTTPFirewallRule{{Id: 1, Param: "${requestPath}", Operator: RuleOperatorMatch, Value: expression}},
				}},
			}},
		},
	}
}

func TestSecurityPolicyValidationRetainsLastValidWAF(t *testing.T) {
	manager := NewWAFManager()
	valid := securityTestPolicy(1, "safe")
	if err := manager.UpdatePolicies([]*firewallconfigs.HTTPFirewallPolicy{valid}); err != nil {
		t.Fatal(err)
	}
	previous := manager.FindWAF(1)
	if previous == nil {
		t.Fatal("valid policy missing")
	}
	if err := manager.ValidatePolicies([]*firewallconfigs.HTTPFirewallPolicy{valid, valid}); err != nil {
		t.Fatalf("shared site policy rejected as duplicate: %v", err)
	}
	if valid.Mode != "" {
		t.Fatal("compiling mutated input policy")
	}
	invalid := securityTestPolicy(1, "[")
	if err := manager.ValidatePolicies([]*firewallconfigs.HTTPFirewallPolicy{invalid}); err == nil {
		t.Fatal("invalid regex validated")
	}
	if err := manager.UpdatePolicies([]*firewallconfigs.HTTPFirewallPolicy{invalid, securityTestPolicy(2, "new")}); err == nil {
		t.Fatal("invalid update accepted")
	}
	if manager.FindWAF(1) != previous || manager.FindWAF(2) != nil {
		t.Fatal("failed update changed active policies")
	}
	if compiled, err := manager.ConvertWAF(invalid); err == nil || compiled != nil {
		t.Fatal("conversion exposed invalid policy")
	}
	if err := manager.UpdatePolicies([]*firewallconfigs.HTTPFirewallPolicy{nil}); err == nil {
		t.Fatal("nil policy accepted")
	}
	if manager.FindWAF(1) != previous {
		t.Fatal("nil update replaced valid policy")
	}
}

func TestSecurityWAFInitReturnsRuleAndActionErrors(t *testing.T) {
	compiled := &WAF{Inbound: []*RuleGroup{{
		Id: 1, IsOn: true,
		RuleSets: []*RuleSet{{Id: 1, IsOn: true, Rules: []*Rule{{Param: "${requestPath}", Operator: RuleOperatorMatch, Value: "["}}}},
	}}}
	if failures := compiled.Init(); len(failures) == 0 {
		t.Fatal("rule initialization error discarded")
	}
	compiled = &WAF{Inbound: []*RuleGroup{{
		Id: 1, IsOn: true,
		RuleSets: []*RuleSet{{Id: 1, IsOn: true, Actions: []*ActionConfig{{Code: "unknown-security-action"}}}},
	}}}
	if failures := compiled.Init(); len(failures) == 0 {
		t.Fatal("unknown action accepted")
	}
}

func TestSecurityBodyRuleInspectsUnknownLengthAndRejectsMalformedJSON(t *testing.T) {
	rule := &Rule{Param: "${requestBody}", Operator: RuleOperatorContains, Value: "blocked-marker"}
	if err := rule.Init(); err != nil {
		t.Fatal(err)
	}
	raw, err := http.NewRequest(http.MethodPost, "http://example.test/", strings.NewReader("blocked-marker"))
	if err != nil {
		t.Fatal(err)
	}
	raw.ContentLength = -1
	matched, _, err := rule.MatchRequest(requests.NewTestRequest(raw))
	if err != nil || !matched {
		t.Fatalf("chunked body match = %t, %v", matched, err)
	}
	_ = raw.Body.Close()

	rule = &Rule{Param: "${requestJSON.name}", Operator: RuleOperatorContains, Value: "blocked"}
	if err := rule.Init(); err != nil {
		t.Fatal(err)
	}
	raw, _ = http.NewRequest(http.MethodPost, "http://example.test/", strings.NewReader(`{"name":`))
	raw.ContentLength = -1
	_, _, err = rule.MatchRequest(requests.NewTestRequest(raw))
	var inspectionErr *requests.InspectionError
	if !errors.As(err, &inspectionErr) {
		t.Fatalf("malformed JSON treated as inspected: %v", err)
	}
	_ = raw.Body.Close()

	raw, _ = http.NewRequest(http.MethodGet, "http://example.test/", nil)
	matched, _, err = rule.MatchRequest(requests.NewTestRequest(raw))
	if err != nil || matched {
		t.Fatalf("ordinary GET rejected by JSON-only check: matched=%t err=%v", matched, err)
	}
}
