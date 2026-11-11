package nodes

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/nodeutils"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/shared"
	"github.com/TeaOSLab/EdgeNode/internal/caches"
	"github.com/TeaOSLab/EdgeNode/internal/httpcache"
	"github.com/TeaOSLab/EdgeNode/internal/utils/fasttime"
)

func TestSecurityPrivateAtomicNodeConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "node.json")
	config := &nodeconfigs.NodeConfig{NodeId: "node-a", Secret: "0123456789abcdef0123456789abcdef"}
	payload, _ := json.Marshal(config)
	if err := persistNodeConfig(filename, config, payload); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filename)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("secret file mode: %v", info.Mode())
	}
	dirInfo, _ := os.Stat(dir)
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("secret directory mode: %v", dirInfo.Mode())
	}
	data, _ := os.ReadFile(filename)
	header, encrypted, ok := strings.Cut(string(data), "\n")
	decoded, _ := base64.StdEncoding.DecodeString(header)
	if !ok || string(decoded) != config.NodeId+"|"+config.Secret {
		t.Fatal("incompatible configuration header")
	}
	plain, err := nodeutils.DecryptData(config.NodeId, config.Secret, encrypted)
	if err != nil || string(plain) != string(payload) {
		t.Fatalf("configuration decode failed: %s %v", plain, err)
	}
	if err := persistNodeConfig(filename, config, []byte("broken")); err == nil {
		t.Fatal("invalid candidate was accepted")
	}
	after, _ := os.ReadFile(filename)
	if string(after) != string(data) {
		t.Fatal("invalid candidate overwrote last known good configuration")
	}
}

func TestSecurityNodeSnapshotDoesNotAliasPreviousServers(t *testing.T) {
	previous := &nodeconfigs.NodeConfig{Servers: []*serverconfigs.ServerConfig{{Id: 7, Name: "previous"}}}
	next, err := cloneNodeSnapshot(previous)
	if err != nil {
		t.Fatal(err)
	}
	next.Servers[0].Name = "next"
	if previous.Servers[0].Name != "previous" {
		t.Fatal("candidate mutated the active server graph")
	}
}

func TestSecurityOriginClientsAreBoundedAndConcurrent(t *testing.T) {
	pool := &HTTPClientPool{clientsMap: map[uint64]*HTTPClient{}, maxClients: 4}
	origin := &serverconfigs.OriginConfig{Id: 3, Addr: &serverconfigs.NetworkAddressConfig{Host: "127.0.0.1", PortRange: "80"}}
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://host"+strings.Repeat("x", i)+".test/a", nil)
		if _, err := pool.Client(&HTTPRequest{RawReq: req}, origin, "127.0.0.1:80", nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(pool.clientsMap) != 4 {
		t.Fatalf("client count=%d, want 4", len(pool.clientsMap))
	}
	var wg sync.WaitGroup
	for _, client := range pool.clientsMap {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(c *HTTPClient) {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					c.UpdateAccessTime()
					_ = c.AccessTime()
				}
			}(client)
		}
	}
	wg.Wait()
	pool.pruneClients(fasttime.Now().Unix() + 1000)
	if len(pool.clientsMap) != 0 {
		t.Fatal("idle client objects were retained")
	}
}

func TestSecurityProxyTransportSnapshotsRequestMetadata(t *testing.T) {
	req := &HTTPRequest{RawReq: httptest.NewRequest(http.MethodGet, "http://example.test/", nil), web: &serverconfigs.HTTPWebConfig{}}
	req.RawReq.RemoteAddr = "192.0.2.4:12345"
	config := &serverconfigs.ProxyProtocolConfig{IsOn: true, Version: serverconfigs.ProxyProtocolVersion1}
	header, err := makeOriginProxyHeader(req, config)
	if err != nil {
		t.Fatal(err)
	}
	req.RawReq.RemoteAddr = "198.51.100.8:54321"
	if header.SourceAddr.String() != "192.0.2.4:12345" {
		t.Fatalf("transport retained mutable request: %v", header.SourceAddr)
	}
}

func TestSecurityStoredCacheVariantRejectsDifferentOrigin(t *testing.T) {
	const policyID = 87431
	// Seed an actual memory storage entry and verify its persisted representation.
	policy := &serverconfigs.HTTPCachePolicy{Id: policyID, IsOn: true, Type: serverconfigs.CachePolicyStorageMemory, Capacity: shared.NewSizeCapacity(1, shared.SizeCapacityUnitMB)}
	storage := caches.NewMemoryStorage(policy, nil)
	if err := policy.Init(); err != nil {
		t.Fatal(err)
	}
	if err := storage.Init(); err != nil {
		t.Fatal(err)
	}
	defer storage.Stop()
	metadata, ok := httpcache.EncodeVary(http.Header{"Vary": {"Origin"}}, http.Header{"Origin": {"https://a.test"}})
	if !ok {
		t.Fatal("metadata")
	}
	writer, err := storage.OpenWriter("variant", time.Now().Unix()+60, 200, -1, 5, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.WriteHeader([]byte("Vary:Origin\n" + httpcache.VaryMetadataHeader + ":" + metadata + "\n"))
	_, _ = writer.Write([]byte("hello"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	storage.AddToList(&caches.Item{Key: "variant", ExpiresAt: time.Now().Unix() + 60, BodySize: 5})
	reader, err := storage.OpenReader("variant", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	buf := make([]byte, 4096)
	var header strings.Builder
	if err := reader.ReadHeader(buf, func(n int) (bool, error) { header.Write(buf[:n]); return true, nil }); err != nil {
		t.Fatal(err)
	}
	stored := make(http.Header)
	for _, line := range strings.Split(strings.TrimSpace(header.String()), "\n") {
		name, value, _ := strings.Cut(line, ":")
		stored.Add(name, value)
	}
	if httpcache.MatchesVary(stored, http.Header{"Origin": {"https://b.test"}}) {
		t.Fatal("cached body was reusable across origins")
	}
}
