package nodes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

type poolCloseProbe struct {
	pool     *HTTPClientPool
	closed   bool
	unlocked bool
}

func (*poolCloseProbe) RoundTrip(*http.Request) (*http.Response, error) {
	panic("no network request is expected")
}

func (p *poolCloseProbe) CloseIdleConnections() {
	p.closed = true
	p.unlocked = p.pool.locker.TryLock()
	if p.unlocked {
		p.pool.locker.Unlock()
	}
}

func poolTestOrigin(t testing.TB) *serverconfigs.OriginConfig {
	t.Helper()
	origin := &serverconfigs.OriginConfig{Id: 7, Addr: &serverconfigs.NetworkAddressConfig{Host: "127.0.0.1", PortRange: "80"}}
	if err := origin.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestSecurityOriginEvictionClosesIdleConnectionsOutsideRegistryLock(t *testing.T) {
	pool := &HTTPClientPool{clientsMap: map[uint64]*HTTPClient{}, maxClients: 3}
	probe := &poolCloseProbe{pool: pool}
	for i := uint64(1); i <= 3; i++ {
		transport := &poolCloseProbe{pool: pool}
		if i == 1 {
			transport = probe
		}
		client := NewHTTPClient(&http.Client{Transport: transport}, false)
		client.accessAt.Store(int64(i))
		pool.clientsMap[i] = client
	}
	client, err := pool.Client(&HTTPRequest{RawReq: httptest.NewRequest(http.MethodGet, "http://new-origin.test/", nil)}, poolTestOrigin(t), "127.0.0.1:80", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if !probe.closed || !probe.unlocked {
		t.Fatalf("evicted transport close: called=%t registry unlocked=%t", probe.closed, probe.unlocked)
	}
	if len(pool.clientsMap) != 3 || pool.clientsMap[1] != nil {
		t.Fatal("oldest sampled entry was not replaced within the capacity")
	}
}

func TestSecurityOriginPoolBoundsConcurrentChurn(t *testing.T) {
	const capacity = 32
	pool := &HTTPClientPool{clientsMap: map[uint64]*HTTPClient{}, maxClients: capacity}
	origin := poolTestOrigin(t)
	var wg sync.WaitGroup
	var failures atomic.Int64
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 128; i++ {
				req := &HTTPRequest{RawReq: httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://host-%d-%d.test/", worker, i), nil)}
				if _, err := pool.Client(req, origin, "127.0.0.1:80", nil, false); err != nil {
					failures.Add(1)
				}
				pool.locker.RLock()
				if len(pool.clientsMap) > capacity {
					failures.Add(1)
				}
				pool.locker.RUnlock()
			}
		}()
	}
	wg.Wait()
	for _, client := range pool.clientsMap {
		client.Close()
	}
	if failures.Load() != 0 || len(pool.clientsMap) != capacity {
		t.Fatalf("concurrent churn failures=%d clients=%d", failures.Load(), len(pool.clientsMap))
	}
}

func TestSecurityOriginIdleDefaultsRespectEffectiveCPUAndExplicitLimits(t *testing.T) {
	tests := []struct {
		name string
		id   int64
		idle int
		want int
	}{
		{"ordinary-default", 7, 0, min(max(runtime.GOMAXPROCS(0), 1), 8) * 16},
		{"internal-default", 0, 0, 128},
		{"ordinary-explicit", 7, 333, 333},
		{"internal-explicit", 0, 333, 333 * 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool := &HTTPClientPool{clientsMap: map[uint64]*HTTPClient{}, maxClients: 4}
			origin := poolTestOrigin(t)
			origin.Id, origin.MaxIdleConns = tc.id, tc.idle
			client, err := pool.Client(&HTTPRequest{RawReq: httptest.NewRequest(http.MethodGet, "http://origin.test/", nil)}, origin, "127.0.0.1:80", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			transport := client.Transport.(*HTTPClientTransport).Transport
			if transport.MaxIdleConns != tc.want || transport.MaxIdleConnsPerHost != tc.want {
				t.Fatalf("idle connection budget=%d/%d, want %d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost, tc.want)
			}
		})
	}
}
