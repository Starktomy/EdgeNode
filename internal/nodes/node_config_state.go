package nodes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/nodeutils"
	"github.com/TeaOSLab/EdgeNode/internal/utils/atomicfile"
	"github.com/TeaOSLab/EdgeNode/internal/waf"
)

var nodeConfigState atomic.Pointer[nodeconfigs.NodeConfig]

func currentNodeConfig() *nodeconfigs.NodeConfig { return nodeConfigState.Load() }

func storeNodeConfig(config *nodeconfigs.NodeConfig) {
	nodeconfigs.ResetNodeConfig(config)
	nodeConfigState.Store(config)
}

func validateNodeWAF(config *nodeconfigs.NodeConfig) error {
	return waf.SharedWAFManager.ValidatePolicies(config.FindAllFirewallPolicies())
}

func validateNodeCandidate(config *nodeconfigs.NodeConfig) error {
	if config == nil {
		return errors.New("node configuration unavailable")
	}
	if err := validateNodeWAF(config); err != nil {
		return fmt.Errorf("invalid WAF configuration: %w", err)
	}
	zone := config.TimeZone
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf("invalid node time zone: %w", err)
	}
	return nil
}

// persistNodeConfig retains the existing encrypted node.json wire format while
// using a private, fsynced temporary file and atomic replacement. rawJSON is the
// received declarative configuration, before Init expands runtime references.
func persistNodeConfig(filename string, config *nodeconfigs.NodeConfig, rawJSON []byte) error {
	if config.NodeId == "" || config.Secret == "" || !json.Valid(rawJSON) {
		return errors.New("invalid node configuration")
	}
	encoded, err := nodeutils.EncryptData(config.NodeId, config.Secret, rawJSON)
	if err != nil {
		return fmt.Errorf("encrypt node configuration: %w", err)
	}
	header := base64.StdEncoding.EncodeToString([]byte(config.NodeId+"|"+config.Secret)) + "\n"
	return atomicfile.Write(filename, append([]byte(header), []byte(encoded)...))
}

func cloneNodeSnapshot(config *nodeconfigs.NodeConfig) (*nodeconfigs.NodeConfig, error) {
	if config == nil {
		return nil, errors.New("node configuration unavailable")
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	var copy nodeconfigs.NodeConfig
	if err := json.Unmarshal(data, &copy); err != nil {
		return nil, err
	}
	return &copy, nil
}

// updateNodeSnapshot rebuilds derived maps before publication. Existing requests
// keep their prior graph rather than observing in-place task mutations.
func (this *Node) updateNodeSnapshot(change func(*nodeconfigs.NodeConfig) error) error {
	this.locker.Lock()
	defer this.locker.Unlock()
	copy, err := cloneNodeSnapshot(currentNodeConfig())
	if err != nil {
		return err
	}
	if err := change(copy); err != nil {
		return err
	}
	if err, servers := copy.Init(context.Background()); err != nil {
		return err
	} else if len(servers) > 0 {
		return errors.New("invalid updated node configuration")
	}
	return this.onReload(copy, false)
}
