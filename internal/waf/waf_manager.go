package waf

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/firewallconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
)

var SharedWAFManager = NewWAFManager()

// WAFManager WAF管理器
type WAFManager struct {
	mapping      map[int64]*WAF // policyId => WAF
	locker       sync.RWMutex
	updateLocker sync.Mutex
}

// NewWAFManager 获取新对象
func NewWAFManager() *WAFManager {
	return &WAFManager{
		mapping: map[int64]*WAF{},
	}
}

// UpdatePolicies 更新策略
func (this *WAFManager) UpdatePolicies(policies []*firewallconfigs.HTTPFirewallPolicy) error {
	this.updateLocker.Lock()
	defer this.updateLocker.Unlock()

	mapping, err := this.compilePolicies(policies)
	if err != nil {
		remotelogs.Error("WAF", "reject policy update: "+err.Error())
		return err
	}
	this.locker.Lock()
	this.mapping = mapping
	this.locker.Unlock()
	return nil
}

// ValidatePolicies compiles a candidate without changing the active policies.
func (this *WAFManager) ValidatePolicies(policies []*firewallconfigs.HTTPFirewallPolicy) error {
	_, err := this.compilePolicies(policies)
	return err
}

func (this *WAFManager) compilePolicies(policies []*firewallconfigs.HTTPFirewallPolicy) (map[int64]*WAF, error) {
	var mapping = make(map[int64]*WAF, len(policies))
	var seen = make(map[int64]*firewallconfigs.HTTPFirewallPolicy, len(policies))
	var failures []error
	for index, policy := range policies {
		if policy == nil {
			failures = append(failures, fmt.Errorf("policy at index %d is nil", index))
			continue
		}
		if previous, exists := seen[policy.Id]; exists {
			// Shared policies may be referenced by multiple sites or locations.
			if !reflect.DeepEqual(previous, policy) {
				failures = append(failures, fmt.Errorf("conflicting policies share id %d", policy.Id))
			}
			continue
		}
		seen[policy.Id] = policy
		compiled, err := this.ConvertWAF(policy)
		if err != nil {
			failures = append(failures, fmt.Errorf("initialize policy %s: %w", strconv.FormatInt(policy.Id, 10), err))
			continue
		}
		mapping[policy.Id] = compiled
	}
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return mapping, nil
}

// FindWAF 查找WAF
func (this *WAFManager) FindWAF(policyId int64) *WAF {
	this.locker.RLock()
	var w = this.mapping[policyId]
	this.locker.RUnlock()
	return w
}

// ConvertWAF 将Policy转换为WAF
func (this *WAFManager) ConvertWAF(policy *firewallconfigs.HTTPFirewallPolicy) (*WAF, error) {
	if policy == nil {
		return nil, errors.New("policy should not be nil")
	}
	var mode = policy.Mode
	if len(mode) == 0 {
		mode = firewallconfigs.FirewallModeDefend
	}
	var w = &WAF{
		Id:               policy.Id,
		IsOn:             policy.IsOn,
		Name:             policy.Name,
		Mode:             mode,
		UseLocalFirewall: policy.UseLocalFirewall,
		SYNFlood:         policy.SYNFlood,
	}

	// inbound
	if policy.Inbound != nil && policy.Inbound.IsOn {
		// ip lists
		if policy.Inbound.AllowListRef != nil && policy.Inbound.AllowListRef.IsOn && policy.Inbound.AllowListRef.ListId > 0 {
			w.AllowListId = policy.Inbound.AllowListRef.ListId
		}

		if policy.Inbound.DenyListRef != nil && policy.Inbound.DenyListRef.IsOn && policy.Inbound.DenyListRef.ListId > 0 {
			w.DenyListId = policy.Inbound.DenyListRef.ListId
		}

		if policy.Inbound.GreyListRef != nil && policy.Inbound.GreyListRef.IsOn && policy.Inbound.GreyListRef.ListId > 0 {
			w.GreyListId = policy.Inbound.GreyListRef.ListId
		}

		// groups
		for _, group := range policy.Inbound.Groups {
			if group == nil {
				return nil, errors.New("inbound rule group should not be nil")
			}
			g := &RuleGroup{
				Id:          group.Id,
				IsOn:        group.IsOn,
				Name:        group.Name,
				Description: group.Description,
				Code:        group.Code,
				IsInbound:   true,
			}

			// rule sets
			for _, set := range group.Sets {
				if set == nil {
					return nil, errors.New("inbound rule set should not be nil")
				}
				var s = &RuleSet{
					Id:                 set.Id,
					Code:               set.Code,
					IsOn:               set.IsOn,
					Name:               set.Name,
					Description:        set.Description,
					Connector:          set.Connector,
					IgnoreLocal:        set.IgnoreLocal,
					IgnoreSearchEngine: set.IgnoreSearchEngine,
				}
				for _, a := range set.Actions {
					if a == nil {
						return nil, errors.New("inbound action should not be nil")
					}
					s.AddAction(a.Code, a.Options)
				}

				// rules
				for _, rule := range set.Rules {
					if rule == nil {
						return nil, errors.New("inbound rule should not be nil")
					}
					r := &Rule{
						Id:                rule.Id,
						Description:       rule.Description,
						Param:             rule.Param,
						ParamFilters:      []*ParamFilter{},
						Operator:          rule.Operator,
						Value:             rule.Value,
						IsCaseInsensitive: rule.IsCaseInsensitive,
						CheckpointOptions: rule.CheckpointOptions,
					}

					for _, paramFilter := range rule.ParamFilters {
						if paramFilter == nil {
							return nil, errors.New("parameter filter should not be nil")
						}
						r.ParamFilters = append(r.ParamFilters, &ParamFilter{
							Code:    paramFilter.Code,
							Options: paramFilter.Options,
						})
					}

					s.Rules = append(s.Rules, r)
				}

				g.RuleSets = append(g.RuleSets, s)
			}

			w.Inbound = append(w.Inbound, g)
		}
	}

	// outbound
	if policy.Outbound != nil && policy.Outbound.IsOn {
		for _, group := range policy.Outbound.Groups {
			if group == nil {
				return nil, errors.New("outbound rule group should not be nil")
			}
			g := &RuleGroup{
				Id:          group.Id,
				IsOn:        group.IsOn,
				Name:        group.Name,
				Description: group.Description,
				Code:        group.Code,
				IsInbound:   true,
			}

			// rule sets
			for _, set := range group.Sets {
				if set == nil {
					return nil, errors.New("outbound rule set should not be nil")
				}
				var s = &RuleSet{
					Id:                 set.Id,
					Code:               set.Code,
					IsOn:               set.IsOn,
					Name:               set.Name,
					Description:        set.Description,
					Connector:          set.Connector,
					IgnoreLocal:        set.IgnoreLocal,
					IgnoreSearchEngine: set.IgnoreSearchEngine,
				}

				for _, a := range set.Actions {
					if a == nil {
						return nil, errors.New("outbound action should not be nil")
					}
					s.AddAction(a.Code, a.Options)
				}

				// rules
				for _, rule := range set.Rules {
					if rule == nil {
						return nil, errors.New("outbound rule should not be nil")
					}
					r := &Rule{
						Id:                rule.Id,
						Description:       rule.Description,
						Param:             rule.Param,
						Operator:          rule.Operator,
						Value:             rule.Value,
						IsCaseInsensitive: rule.IsCaseInsensitive,
						CheckpointOptions: rule.CheckpointOptions,
					}
					s.Rules = append(s.Rules, r)
				}

				g.RuleSets = append(g.RuleSets, s)
			}

			w.Outbound = append(w.Outbound, g)
		}
	}

	// block action
	if policy.BlockOptions != nil {
		w.DefaultBlockAction = &BlockAction{
			StatusCode:        policy.BlockOptions.StatusCode,
			Body:              policy.BlockOptions.Body,
			URL:               policy.BlockOptions.URL,
			Timeout:           policy.BlockOptions.Timeout,
			TimeoutMax:        policy.BlockOptions.TimeoutMax,
			FailBlockScopeAll: policy.BlockOptions.FailBlockScopeAll,
		}
	}

	// page action
	if policy.PageOptions != nil {
		w.DefaultPageAction = &PageAction{
			Status: policy.PageOptions.Status,
			Body:   policy.PageOptions.Body,
		}
	}

	// captcha action
	if policy.CaptchaOptions != nil {
		w.DefaultCaptchaAction = &CaptchaAction{
			Life:              policy.CaptchaOptions.Life,
			MaxFails:          policy.CaptchaOptions.MaxFails,
			FailBlockTimeout:  policy.CaptchaOptions.FailBlockTimeout,
			FailBlockScopeAll: policy.CaptchaOptions.FailBlockScopeAll,
			CountLetters:      policy.CaptchaOptions.CountLetters,
			CaptchaType:       policy.CaptchaOptions.CaptchaType,
			UIIsOn:            policy.CaptchaOptions.UIIsOn,
			UITitle:           policy.CaptchaOptions.UITitle,
			UIPrompt:          policy.CaptchaOptions.UIPrompt,
			UIButtonTitle:     policy.CaptchaOptions.UIButtonTitle,
			UIShowRequestId:   policy.CaptchaOptions.UIShowRequestId,
			UICss:             policy.CaptchaOptions.UICss,
			UIFooter:          policy.CaptchaOptions.UIFooter,
			UIBody:            policy.CaptchaOptions.UIBody,
			Lang:              policy.CaptchaOptions.Lang,
			GeeTestConfig:     &policy.CaptchaOptions.GeeTestConfig,
		}
	}

	// get302
	if policy.Get302Options != nil {
		w.DefaultGet302Action = &Get302Action{
			Life:  policy.Get302Options.Life,
			Scope: policy.Get302Options.Scope,
		}
	}

	// post307
	if policy.Post307Options != nil {
		w.DefaultPost307Action = &Post307Action{
			Life:  policy.Post307Options.Life,
			Scope: policy.Post307Options.Scope,
		}
	}

	// jscookie
	if policy.JSCookieOptions != nil {
		w.DefaultJSCookieAction = &JSCookieAction{
			Life:              policy.JSCookieOptions.Life,
			MaxFails:          policy.JSCookieOptions.MaxFails,
			FailBlockTimeout:  policy.JSCookieOptions.FailBlockTimeout,
			Scope:             policy.JSCookieOptions.Scope,
			FailBlockScopeAll: policy.JSCookieOptions.FailBlockScopeAll,
		}
	}

	errorList := w.Init()
	if len(errorList) > 0 {
		return nil, errors.Join(errorList...)
	}

	return w, nil
}
