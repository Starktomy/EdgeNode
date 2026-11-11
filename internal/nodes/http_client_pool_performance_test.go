package nodes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

func BenchmarkOriginPoolLookup(b *testing.B) {
	for _, capacity := range []int{64, 4096} {
		b.Run(fmt.Sprint(capacity), func(b *testing.B) {
			pool := &HTTPClientPool{clientsMap: map[uint64]*HTTPClient{}, maxClients: capacity}
			origin := &serverconfigs.OriginConfig{Id: 7, Addr: &serverconfigs.NetworkAddressConfig{Host: "127.0.0.1", PortRange: "80"}}
			if err := origin.Init(context.Background()); err != nil {
				b.Fatal(err)
			}
			req := &HTTPRequest{RawReq: httptest.NewRequest(http.MethodGet, "http://origin.example.test/data", nil)}
			hosts := make([]string, capacity*2)
			for i := range hosts {
				hosts[i] = fmt.Sprintf("origin-%d.example.test", i)
			}
			for i := 0; i < capacity; i++ {
				req.RawReq.URL.Host = hosts[i]
				if _, err := pool.Client(req, origin, "127.0.0.1:80", nil, false); err != nil {
					b.Fatal(err)
				}
			}
			b.Cleanup(func() {
				for _, client := range pool.clientsMap {
					client.Close()
				}
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req.RawReq.URL.Host = hosts[(capacity+i)%len(hosts)]
				if _, err := pool.Client(req, origin, "127.0.0.1:80", nil, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
