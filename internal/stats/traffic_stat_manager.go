package stats

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
	"github.com/TeaOSLab/EdgeNode/internal/events"
	"github.com/TeaOSLab/EdgeNode/internal/monitor"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/rpc"
	"github.com/TeaOSLab/EdgeNode/internal/utils/fasttime"
	"github.com/TeaOSLab/EdgeNode/internal/utils/goman"
	"github.com/iwind/TeaGo/Tea"
	"github.com/iwind/TeaGo/maps"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var SharedTrafficStatManager = NewTrafficStatManager()

type TrafficItem struct {
	UserId               int64
	Bytes                int64
	CachedBytes          int64
	CountRequests        int64
	CountCachedRequests  int64
	CountAttackRequests  int64
	AttackBytes          int64
	PlanId               int64
	CheckingTrafficLimit bool
}

func (this *TrafficItem) Add(anotherItem *TrafficItem) {
	this.Bytes += anotherItem.Bytes
	this.CachedBytes += anotherItem.CachedBytes
	this.CountRequests += anotherItem.CountRequests
	this.CountCachedRequests += anotherItem.CountCachedRequests
	this.CountAttackRequests += anotherItem.CountAttackRequests
	this.AttackBytes += anotherItem.AttackBytes
}

type trafficBucketKey struct {
	timestamp int64
	serverID  int64
}

type trafficDomainKey struct {
	timestamp int64
	domain    string
}

// TrafficStatManager 区域流量统计
type TrafficStatManager struct {
	itemMap    map[trafficBucketKey]*TrafficItem
	domainsMap map[int64]map[trafficDomainKey]*TrafficItem // serverID => timestamp/domain => item

	batchSpool   *delivery.Spool
	uploadLocker sync.Mutex
	sendBatch    func(context.Context, string, *pb.UploadServerDailyStatsRequest) error

	locker sync.Mutex

	totalRequests        atomic.Int64
	droppedDomainSamples atomic.Uint64
}

const maxBufferedDomainsPerServer = 256

// DroppedDomainSamples counts samples omitted from the optional domain
// breakdown. Server totals are always recorded before this sampling boundary.
func (this *TrafficStatManager) DroppedDomainSamples() uint64 {
	return this.droppedDomainSamples.Load()
}

// NewTrafficStatManager 获取新对象
func NewTrafficStatManager() *TrafficStatManager {
	var manager = &TrafficStatManager{
		itemMap:    map[trafficBucketKey]*TrafficItem{},
		domainsMap: map[int64]map[trafficDomainKey]*TrafficItem{},
		batchSpool: delivery.NewSpool(filepath.Join(Tea.Root, "data", "spool", "traffic"), 256<<20, 16<<20),
	}

	return manager
}

// Start 启动自动任务
func (this *TrafficStatManager) Start() {
	// 上传请求总数
	var monitorTicker = time.NewTicker(1 * time.Minute)
	events.OnKey(events.EventQuit, this, func() {
		monitorTicker.Stop()
	})
	goman.New(func() {
		for range monitorTicker.C {
			if total := this.totalRequests.Swap(0); total > 0 {
				monitor.SharedValueQueue.Add(nodeconfigs.NodeValueItemRequests, maps.Map{"total": total})
			}
		}
	})

	// 上传统计数据
	var duration = 5 * time.Minute
	if Tea.IsTesting() {
		// 测试环境缩短上传时间，方便我们调试
		duration = 30 * time.Second
	}
	var ticker = time.NewTicker(duration)
	events.OnKey(events.EventQuit, this, func() {
		remotelogs.Println("TRAFFIC_STAT_MANAGER", "quit")
		ticker.Stop()
	})
	remotelogs.Println("TRAFFIC_STAT_MANAGER", "start ...")
	for range ticker.C {
		err := this.Upload()
		if err != nil {
			if !rpc.IsConnError(err) {
				remotelogs.Error("TRAFFIC_STAT_MANAGER", "upload stats failed: "+err.Error())
			} else {
				remotelogs.Warn("TRAFFIC_STAT_MANAGER", "upload stats failed: "+err.Error())
			}
		}
	}
}

// Add 添加流量
func (this *TrafficStatManager) Add(userId int64, serverId int64, domain string, bytes int64, cachedBytes int64, countRequests int64, countCachedRequests int64, countAttacks int64, attackBytes int64, countWebsocketConnections int64, checkingTrafficLimit bool, planId int64) {
	if serverId == 0 {
		return
	}

	// 添加到带宽
	SharedBandwidthStatManager.AddTraffic(serverId, cachedBytes, countRequests, countCachedRequests, countAttacks, attackBytes, countWebsocketConnections)

	if bytes == 0 && countRequests == 0 {
		return
	}

	this.totalRequests.Add(1)

	var timestamp = fasttime.Now().UnixFloor(300)
	var key = trafficBucketKey{timestamp: timestamp, serverID: serverId}
	this.locker.Lock()

	// 总的流量
	item, ok := this.itemMap[key]
	if !ok {
		item = &TrafficItem{
			UserId: userId,
		}
		this.itemMap[key] = item
	}
	item.Bytes += bytes
	item.CachedBytes += cachedBytes
	item.CountRequests += countRequests
	item.CountCachedRequests += countCachedRequests
	item.CountAttackRequests += countAttacks
	item.AttackBytes += attackBytes
	item.CheckingTrafficLimit = checkingTrafficLimit
	item.PlanId = planId

	// 单个域名流量
	if len(domain) < 128 {
		var domainKey = trafficDomainKey{timestamp: timestamp, domain: domain}
		serverDomainMap, ok := this.domainsMap[serverId]
		if !ok {
			serverDomainMap = map[trafficDomainKey]*TrafficItem{}
			this.domainsMap[serverId] = serverDomainMap
		}

		domainItem, ok := serverDomainMap[domainKey]
		if !ok {
			// Wildcard sites can receive an unlimited number of attacker-chosen
			// Host values. The existing top-20 report must not allocate one bucket
			// per unique Host before the upload timer runs.
			if len(serverDomainMap) >= maxBufferedDomainsPerServer {
				this.droppedDomainSamples.Add(1)
				this.locker.Unlock()
				return
			}
			domainItem = &TrafficItem{}
			// Copy only a new retained key: a request's Host can be a substring
			// backed by a much larger request buffer.
			domainKey.domain = strings.Clone(domain)
			serverDomainMap[domainKey] = domainItem
		}
		domainItem.Bytes += bytes
		domainItem.CachedBytes += cachedBytes
		domainItem.CountRequests += countRequests
		domainItem.CountCachedRequests += countCachedRequests
		domainItem.CountAttackRequests += countAttacks
		domainItem.AttackBytes += attackBytes
	}

	this.locker.Unlock()
}

// Upload 上传流量
func (this *TrafficStatManager) Upload() error {
	this.uploadLocker.Lock()
	defer this.uploadLocker.Unlock()
	// Always drain durable work before admitting new data. A full disk spool must
	// remain recoverable when the API comes back online.
	sendErr := this.sendPending()
	var regionId int64
	nodeConfig, _ := nodeconfigs.SharedNodeConfig()
	if nodeConfig != nil {
		regionId = nodeConfig.RegionId
	}

	this.locker.Lock()

	var itemMap = this.itemMap
	var domainMap = this.domainsMap

	// reset
	this.itemMap = map[trafficBucketKey]*TrafficItem{}
	this.domainsMap = map[int64]map[trafficDomainKey]*TrafficItem{}

	this.locker.Unlock()

	defer func() {
		{
			// Disk-full / serialization errors must not discard collected traffic.
			this.locker.Lock()
			defer this.locker.Unlock()
			for key, item := range itemMap {
				if current := this.itemMap[key]; current != nil {
					current.Add(item)
				} else {
					this.itemMap[key] = item
				}
			}
			for serverID, domains := range domainMap {
				if len(domains) == 0 {
					continue
				}
				if this.domainsMap[serverID] == nil {
					this.domainsMap[serverID] = map[trafficDomainKey]*TrafficItem{}
				}
				for key, item := range domains {
					if current := this.domainsMap[serverID][key]; current != nil {
						current.Add(item)
					} else if len(this.domainsMap[serverID]) >= maxBufferedDomainsPerServer {
						this.droppedDomainSamples.Add(uint64(max(item.CountRequests, 1)))
					} else {
						this.domainsMap[serverID][key] = item
					}
				}
			}
		}
	}()

	const maxRecords = 1000
	const maxPayload = 1 << 20
	request := &pb.UploadServerDailyStatsRequest{}
	var commits []func()
	var estimatedBytes int
	flush := func() error {
		if len(commits) == 0 {
			return nil
		}
		payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
		if err != nil {
			return err
		}
		id, err := this.batchSpool.Append(payload)
		if id != "" {
			// Rename published the original identity even if directory fsync failed.
			// Never merge these deltas back and assign a new ID on retry.
			for _, commit := range commits {
				commit()
			}
		}
		request = &pb.UploadServerDailyStatsRequest{}
		commits = nil
		estimatedBytes = 0
		return err
	}
	admit := func(size int) error {
		if len(commits) >= maxRecords || estimatedBytes+size+16 > maxPayload {
			return flush()
		}
		return nil
	}
	for key, item := range itemMap {
		stat := &pb.ServerDailyStat{
			UserId: item.UserId, ServerId: key.serverID, NodeRegionId: regionId,
			Bytes: item.Bytes, CachedBytes: item.CachedBytes,
			CountRequests: item.CountRequests, CountCachedRequests: item.CountCachedRequests,
			CountAttackRequests: item.CountAttackRequests, AttackBytes: item.AttackBytes,
			CheckTrafficLimiting: item.CheckingTrafficLimit, PlanId: item.PlanId, CreatedAt: key.timestamp,
		}
		if err := admit(proto.Size(stat)); err != nil {
			return errors.Join(sendErr, err)
		}
		request.Stats = append(request.Stats, stat)
		estimatedBytes += proto.Size(stat) + 16
		commits = append(commits, func() { delete(itemMap, key) })
	}
	// Domain breakdowns intentionally keep the existing top-20 sampling policy;
	// the separate server totals above remain complete for billing.
	const maxDomainsPerServer = 20
	for serverID, domains := range domainMap {
		keys := make([]trafficDomainKey, 0, len(domains))
		for key := range domains {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return domains[keys[i]].CountRequests > domains[keys[j]].CountRequests })
		if len(keys) > maxDomainsPerServer {
			for _, key := range keys[maxDomainsPerServer:] {
				delete(domains, key)
			}
			keys = keys[:maxDomainsPerServer]
		}
		for _, key := range keys {
			item := domains[key]
			stat := &pb.UploadServerDailyStatsRequest_DomainStat{
				ServerId: serverID, Domain: key.domain, Bytes: item.Bytes,
				CachedBytes: item.CachedBytes, CountRequests: item.CountRequests,
				CountCachedRequests: item.CountCachedRequests, CountAttackRequests: item.CountAttackRequests,
				AttackBytes: item.AttackBytes, CreatedAt: key.timestamp,
			}
			if stat.CachedBytes > stat.Bytes || stat.CountCachedRequests == stat.CountRequests {
				stat.CachedBytes = stat.Bytes
			}
			if err := admit(proto.Size(stat)); err != nil {
				return errors.Join(sendErr, err)
			}
			request.DomainStats = append(request.DomainStats, stat)
			estimatedBytes += proto.Size(stat) + 16
			commits = append(commits, func() { delete(domains, key) })
		}
	}
	appendErr := flush()
	// Even an append/capacity failure must not prevent draining existing work.
	return errors.Join(appendErr, this.sendPending())
}

func (this *TrafficStatManager) sendPending() error {
	return this.batchSpool.WalkPending(func(batch delivery.Batch) error {
		var request pb.UploadServerDailyStatsRequest
		decodeErr := delivery.ValidateBatchRecords(batch.Data, request.ProtoReflect().Descriptor(), 1, 2)
		if decodeErr == nil {
			decodeErr = (proto.UnmarshalOptions{RecursionLimit: delivery.MaxBatchDepth}).Unmarshal(batch.Data, &request)
		}
		if err := decodeErr; err != nil {
			if isolateErr := this.batchSpool.Quarantine(batch.ID, "invalid traffic protobuf: "+err.Error()); isolateErr != nil {
				return errors.Join(err, isolateErr)
			}
			remotelogs.Error("TRAFFIC_STAT_MANAGER", fmt.Sprintf("quarantined corrupt traffic batch %s: %v", batch.ID, err))
			return nil
		}
		var err error
		if this.sendBatch != nil {
			err = this.sendBatch(context.Background(), batch.ID, &request)
		} else {
			client, clientErr := rpc.SharedRPC()
			if clientErr != nil {
				return clientErr
			}
			ctx, cancel := context.WithTimeout(client.Context(), 30*time.Second)
			ctx = metadata.AppendToOutgoingContext(ctx, "x-edge-stat-batch", batch.ID)
			_, err = client.ServerDailyStatRPC.UploadServerDailyStats(ctx, &request)
			cancel()
		}
		if err != nil {
			switch status.Code(err) {
			case codes.InvalidArgument, codes.PermissionDenied, codes.DataLoss:
				if isolateErr := this.batchSpool.Quarantine(batch.ID, err.Error()); isolateErr != nil {
					return errors.Join(err, isolateErr)
				}
				remotelogs.Error("TRAFFIC_STAT_MANAGER", fmt.Sprintf("traffic batch %s requires operator review: %v", batch.ID, err))
				return nil
			}
			return err
		}
		return this.batchSpool.Ack(batch.ID)
	})
}
