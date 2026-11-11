package nodes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/shared"
	"github.com/TeaOSLab/EdgeNode/internal/caches"
	"github.com/TeaOSLab/EdgeNode/internal/httpcache"
)

func cacheProtocolFixture(t *testing.T) (*serverconfigs.HTTPCachePolicy, *serverconfigs.HTTPCacheRef, caches.StorageInterface) {
	t.Helper()
	old := caches.SharedManager
	caches.SharedManager = caches.NewManager()
	ref := &serverconfigs.HTTPCacheRef{IsOn: true, Key: "protocol-fixture", Methods: []string{"GET", "HEAD"}}
	if err := ref.Init(); err != nil {
		t.Fatal(err)
	}
	policy := &serverconfigs.HTTPCachePolicy{Id: 93715, IsOn: true, Type: serverconfigs.CachePolicyStorageMemory, Capacity: shared.NewSizeCapacity(1, shared.SizeCapacityUnitMB), CacheRefs: []*serverconfigs.HTTPCacheRef{ref}}
	caches.SharedManager.UpdatePolicies([]*serverconfigs.HTTPCachePolicy{policy})
	storage := caches.SharedManager.FindStorageWithPolicy(policy.Id)
	if storage == nil {
		t.Fatal("memory cache was not initialized")
	}
	t.Cleanup(func() { caches.SharedManager.UpdatePolicies(nil); caches.SharedManager = old })
	return policy, ref, storage
}

func cacheProtocolRequest(policy *serverconfigs.HTTPCachePolicy, ref *serverconfigs.HTTPCacheRef, headers http.Header) (*HTTPRequest, *httptest.ResponseRecorder) {
	raw := httptest.NewRequest(http.MethodGet, "http://cache.test/data", nil)
	raw.Header = headers
	request := &HTTPRequest{RawReq: raw, ReqHost: "cache.test", nodeConfig: &nodeconfigs.NodeConfig{}, ReqServer: &serverconfigs.ServerConfig{Id: 12, HTTPCachePolicy: policy}, web: &serverconfigs.HTTPWebConfig{Cache: &serverconfigs.HTTPCacheConfig{IsOn: true, EnableCacheControlMaxAge: true}}, cacheRef: ref, varMapping: map[string]string{}, logAttrs: map[string]string{}, uri: "/data"}
	recorder := httptest.NewRecorder()
	request.writer = NewHTTPWriter(request, recorder)
	return request, recorder
}

func TestSecurityCacheVaryMismatchCannotLeakCachedHeadersOrBody(t *testing.T) {
	policy, ref, storage := cacheProtocolFixture(t)
	metadata, _ := httpcache.EncodeVary(http.Header{"Vary": {"Origin"}}, http.Header{"Origin": {"https://tenant-a.test"}})
	header := "Vary:Origin\nAccess-Control-Allow-Origin:https://tenant-a.test\nX-Private-Marker:tenant-a\n" + httpcache.VaryMetadataHeader + ":" + metadata + "\n"
	writer, err := storage.OpenWriter(ref.Key, time.Now().Unix()+60, 200, len(header), 6, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.WriteHeader([]byte(header))
	_, _ = writer.Write([]byte("secret"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	storage.AddToList(&caches.Item{Key: ref.Key, ExpiresAt: time.Now().Unix() + 60, BodySize: 6})
	request, recorder := cacheProtocolRequest(policy, ref, http.Header{"Origin": {"https://tenant-b.test"}})
	if request.doCacheRead(false) || recorder.Body.Len() != 0 || recorder.Header().Get("X-Private-Marker") != "" || recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("wrong Vary representation leaked: headers=%v body=%q", recorder.Header(), recorder.Body.String())
	}
	request, recorder = cacheProtocolRequest(policy, ref, http.Header{"Origin": {"https://tenant-a.test"}})
	if !request.doCacheRead(false) {
		t.Fatal("matching cached representation was bypassed")
	}
	request.writer.Close()
	if recorder.Body.String() != "secret" || recorder.Header().Get(httpcache.VaryMetadataHeader) != "" {
		t.Fatalf("cache body or internal metadata handling failed: %q %v", recorder.Body.String(), recorder.Header())
	}
}

func TestSecurityCacheMaxAgeZeroDoesNotOpenStorageWriter(t *testing.T) {
	policy, ref, _ := cacheProtocolFixture(t)
	request, _ := cacheProtocolRequest(policy, ref, make(http.Header))
	request.cacheKey = ref.Key
	request.cacheRequestHeader = request.RawReq.Header.Clone()
	request.writer.Header().Set("Cache-Control", "public, max-age=0")
	request.writer.statusCode = http.StatusOK
	request.writer.PrepareCache(&http.Response{Body: io.NopCloser(strings.NewReader("body")), Header: request.writer.Header(), StatusCode: 200}, 4)
	if request.writer.cacheWriter != nil {
		t.Fatal("max-age=0 response opened a persistent cache entry")
	}
}

func TestSecurityColdCacheWaitersReadCommittedEntry(t *testing.T) {
	policy, ref, storage := cacheProtocolFixture(t)
	leader, _ := cacheProtocolRequest(policy, ref, make(http.Header))
	if leader.doCacheRead(false) || leader.cacheFillRelease == nil {
		t.Fatal("cold cache did not register a producer")
	}
	var wg sync.WaitGroup
	started := make(chan struct{}, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, recorder := cacheProtocolRequest(policy, ref, make(http.Header))
			started <- struct{}{}
			if !request.doCacheRead(false) {
				t.Errorf("follower repeated origin work after a successful fill: %v", request.varMapping)
				request.writer.Close()
				return
			}
			request.writer.Close()
			if recorder.Body.String() != "cached" {
				t.Errorf("follower read uncommitted/incorrect bytes: %q", recorder.Body.String())
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-started
	}
	header := "Content-Type:text/plain\n"
	writer, err := storage.OpenWriter(ref.Key, time.Now().Unix()+60, 200, len(header), 6, -1, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.WriteHeader([]byte(header))
	_, _ = writer.Write([]byte("cached"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	storage.AddToList(&caches.Item{Key: ref.Key, ExpiresAt: time.Now().Unix() + 60, BodySize: 6})
	leader.writer.Close()
	wg.Wait()
}
