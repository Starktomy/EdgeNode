package nodes

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeNode/internal/utils/goman"
	"github.com/cespare/xxhash/v2"
	"github.com/pires/go-proxyproto"
	"golang.org/x/net/http2"
)

// SharedHTTPClientPool HTTP客户端池单例
var SharedHTTPClientPool = NewHTTPClientPool()

const httpClientProxyProtocolTag = "@ProxyProtocol@"
const maxHTTPRedirects = 8
const defaultMaxHTTPClients = 4096

// HTTPClientPool 客户端池
type HTTPClientPool struct {
	clientsMap map[uint64]*HTTPClient // origin key => client

	cleanTicker *time.Ticker
	maxClients  int

	locker sync.RWMutex
}

// NewHTTPClientPool 获取新对象
func NewHTTPClientPool() *HTTPClientPool {
	var pool = &HTTPClientPool{
		cleanTicker: time.NewTicker(1 * time.Minute),
		clientsMap:  map[uint64]*HTTPClient{},
		maxClients:  defaultMaxHTTPClients,
	}

	goman.New(func() {
		pool.cleanClients()
	})

	return pool
}

// Client 根据地址获取客户端
func (this *HTTPClientPool) Client(req *HTTPRequest,
	origin *serverconfigs.OriginConfig,
	originAddr string,
	proxyProtocol *serverconfigs.ProxyProtocolConfig,
	followRedirects bool) (rawClient *http.Client, err error) {
	if origin == nil {
		return nil, errors.New("origin must not be nil")
	}
	if origin.Addr == nil {
		return nil, errors.New("origin addr should not be empty (originId:" + strconv.FormatInt(origin.Id, 10) + ")")
	}

	if req == nil || req.RawReq == nil || req.RawReq.URL == nil {
		err = errors.New("invalid request url")
		return
	}
	var originHost = req.RawReq.URL.Host
	var urlPort = req.RawReq.URL.Port()
	if len(urlPort) == 0 {
		if req.RawReq.URL.Scheme == "http" {
			urlPort = "80"
		} else {
			urlPort = "443"
		}

		originHost += ":" + urlPort
	}

	var rawKey = origin.UniqueKey() + "@" + originAddr + "@" + originHost

	// if we are under available ProxyProtocol, we add client ip to key to make every client unique
	var isProxyProtocol = false
	var proxyHeader *proxyproto.Header
	if proxyProtocol != nil && proxyProtocol.IsOn {
		proxyHeader, err = makeOriginProxyHeader(req, proxyProtocol)
		if err != nil {
			return nil, err
		}
		rawKey += httpClientProxyProtocolTag + strconv.Itoa(int(proxyHeader.Version)) + "@" + proxyHeader.SourceAddr.String()
		if proxyHeader.DestinationAddr != nil {
			rawKey += "@" + proxyHeader.DestinationAddr.String()
		}
		isProxyProtocol = true
	}

	// follow redirects
	if followRedirects {
		rawKey += "@follow"
	}

	var key = xxhash.Sum64String(rawKey)

	var isLnRequest = origin.Id == 0

	this.locker.RLock()
	client, found := this.clientsMap[key]
	this.locker.RUnlock()
	if found {
		client.UpdateAccessTime()
		return client.RawClient(), nil
	}

	// 这里不能使用RLock，避免因为并发生成多个同样的client实例
	this.locker.Lock()
	defer this.locker.Unlock()

	// 再次查找
	client, found = this.clientsMap[key]
	if found {
		client.UpdateAccessTime()
		return client.RawClient(), nil
	}

	if this.maxClients <= 0 {
		this.maxClients = defaultMaxHTTPClients
	}
	if len(this.clientsMap) >= this.maxClients {
		var oldestKey uint64
		var oldest *HTTPClient
		for candidateKey, candidate := range this.clientsMap {
			if oldest == nil || candidate.AccessTime() < oldest.AccessTime() {
				oldestKey, oldest = candidateKey, candidate
			}
		}
		if oldest != nil {
			delete(this.clientsMap, oldestKey)
			oldest.Close()
		}
	}

	var maxConnections = origin.MaxConns
	var connectionTimeout = origin.ConnTimeoutDuration()
	var readTimeout = origin.ReadTimeoutDuration()
	var idleTimeout = origin.IdleTimeoutDuration()
	var idleConns = origin.MaxIdleConns

	// 超时时间
	if connectionTimeout <= 0 {
		connectionTimeout = 15 * time.Second
	}

	if idleTimeout <= 0 {
		idleTimeout = 2 * time.Minute
	}

	var numberCPU = runtime.NumCPU()
	if numberCPU < 8 {
		numberCPU = 8
	}
	if maxConnections <= 0 {
		maxConnections = numberCPU * 64
	}

	if idleConns <= 0 {
		idleConns = numberCPU * 16
	}

	if isProxyProtocol { // ProxyProtocol无需保持太多空闲连接
		idleConns = 3
	} else if isLnRequest { // 可以判断为Ln节点请求
		maxConnections *= 8
		idleConns *= 8
		idleTimeout *= 4
	} else if currentNodeConfig() != nil && currentNodeConfig().Level > 1 {
		// Ln节点可以适当增加连接数
		maxConnections *= 2
		idleConns *= 2
	}

	// TLS通讯
	tlsConfig, err := originTLSConfig(origin, req.RawReq.URL.Hostname())
	if err != nil {
		return nil, err
	}

	var transport = &HTTPClientTransport{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				var realAddr = originAddr

				// for redirections
				if followRedirects && originHost != addr {
					realAddr = addr
				}

				// connect
				conn, dialErr := (&net.Dialer{
					Timeout:   connectionTimeout,
					KeepAlive: 1 * time.Minute,
				}).DialContext(ctx, network, realAddr)
				if dialErr != nil {
					return nil, dialErr
				}

				// handle PROXY protocol
				if proxyHeader != nil {
					header := *proxyHeader
					if header.DestinationAddr == nil {
						header.DestinationAddr = conn.RemoteAddr()
					}
					if _, proxyErr := header.WriteTo(conn); proxyErr != nil {
						_ = conn.Close()
						return nil, proxyErr
					}
				}

				return NewOriginConn(conn), nil
			},
			MaxIdleConns:          idleConns,
			MaxIdleConnsPerHost:   idleConns,
			MaxConnsPerHost:       maxConnections,
			IdleConnTimeout:       idleTimeout,
			ExpectContinueTimeout: 1 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			TLSClientConfig:       tlsConfig,
			ReadBufferSize:        8 * 1024,
			Proxy:                 nil,
		},
	}

	// support http/2
	if origin.HTTP2Enabled && origin.Addr != nil && origin.Addr.Protocol == serverconfigs.ProtocolHTTPS {
		_ = http2.ConfigureTransport(transport.Transport)
	}

	rawClient = &http.Client{
		Timeout:   readTimeout,
		Transport: transport,
		CheckRedirect: func(targetReq *http.Request, via []*http.Request) error {
			// follow redirects
			if followRedirects && len(via) <= maxHTTPRedirects {
				return nil
			}

			return http.ErrUseLastResponse
		},
	}

	this.clientsMap[key] = NewHTTPClient(rawClient, isProxyProtocol)

	return rawClient, nil
}

// 清理不使用的Client
func (this *HTTPClientPool) cleanClients() {
	for now := range this.cleanTicker.C {
		this.pruneClients(now.Unix())
	}
}

func (this *HTTPClientPool) pruneClients(now int64) {
	var expired []*HTTPClient
	this.locker.Lock()
	for key, client := range this.clientsMap {
		idleSeconds := int64(600)
		if client.IsProxyProtocol() {
			idleSeconds = 120
		}
		if client.AccessTime() < now-idleSeconds {
			delete(this.clientsMap, key)
			expired = append(expired, client)
		}
	}
	this.locker.Unlock()
	for _, client := range expired {
		client.Close()
	}
}

// makeOriginProxyHeader snapshots connection metadata without retaining the
// request, its body, ResponseWriter, or configuration snapshot in the transport.
func makeOriginProxyHeader(req *HTTPRequest, config *serverconfigs.ProxyProtocolConfig) (*proxyproto.Header, error) {
	if config.Version != serverconfigs.ProxyProtocolVersion1 && config.Version != serverconfigs.ProxyProtocolVersion2 {
		return nil, errors.New("unsupported PROXY protocol version")
	}
	remoteIP := net.ParseIP(req.requestRemoteAddr(true))
	if remoteIP == nil {
		return nil, errors.New("invalid PROXY protocol client IP")
	}
	protocol := proxyproto.TCPv4
	if remoteIP.To4() == nil {
		protocol = proxyproto.TCPv6
	}
	var destination net.Addr
	if conn, ok := req.RawReq.Context().Value(HTTPConnContextKey).(net.Conn); ok {
		if local, ok := conn.LocalAddr().(*net.TCPAddr); ok {
			destination = &net.TCPAddr{IP: append(net.IP(nil), local.IP...), Port: local.Port, Zone: local.Zone}
		}
	}
	return &proxyproto.Header{
		Version:           byte(config.Version),
		Command:           proxyproto.PROXY,
		TransportProtocol: protocol,
		SourceAddr:        &net.TCPAddr{IP: remoteIP, Port: req.requestRemotePort()},
		DestinationAddr:   destination,
	}, nil
}
