// Copyright 2021 GoEdge goedge.cdn@gmail.com. All rights reserved.

package nodes

import (
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/healthcheck"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

var healthProbeReplay = struct {
	sync.Mutex
	used map[[sha256.Size]byte]time.Time
}{used: map[[sha256.Size]byte]time.Time{}}

func acceptHealthProbe(token string) bool {
	healthProbeReplay.Lock()
	defer healthProbeReplay.Unlock()
	var now = time.Now()
	for tokenHash, expires := range healthProbeReplay.used {
		if !expires.After(now) {
			delete(healthProbeReplay.used, tokenHash)
		}
	}
	var hash = sha256.Sum256([]byte(token))
	if _, used := healthProbeReplay.used[hash]; used || len(healthProbeReplay.used) >= 4096 {
		return false
	}
	healthProbeReplay.used[hash] = now.Add(65 * time.Second)
	return true
}

// 健康检查
func (this *HTTPRequest) doHealthCheck(key string, isHealthCheck *bool) (stop bool) {
	this.RawReq.Header.Del(serverconfigs.HealthCheckHeaderName)
	if this.nodeConfig == nil {
		return false
	}
	onlyBasic, accessLog, err := healthcheck.Verify(key, this.nodeConfig.NodeId, this.nodeConfig.Secret, this.RawReq)
	if err != nil || !acceptHealthProbe(key) {
		// Unauthenticated or replayed claims remain ordinary requests. In
		// particular they must not disable WAF, UAM, logging or domain checks.
		return false
	}
	this.tags = append(this.tags, "healthCheck")
	*isHealthCheck = true

	this.web.StatRef = nil

	if !accessLog {
		this.disableLog = true
	}

	if onlyBasic {
		this.writer.WriteHeader(http.StatusOK)
		return true
	}

	return
}
