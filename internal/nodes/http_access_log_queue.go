package nodes

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
	"github.com/TeaOSLab/EdgeNode/internal/events"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/rpc"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/TeaOSLab/EdgeNode/internal/utils/goman"
	memutils "github.com/TeaOSLab/EdgeNode/internal/utils/mem"
	"github.com/iwind/TeaGo/Tea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var sharedHTTPAccessLogQueue = NewHTTPAccessLogQueue()

type accessLogFlushRequest struct {
	ctx  context.Context
	done chan error
}

// HTTPAccessLogQueue HTTP访问日志队列
type HTTPAccessLogQueue struct {
	queue chan *pb.HTTPAccessLog

	rpcClient     *rpc.RPCClient
	wake          chan struct{}
	flush         chan accessLogFlushRequest
	viewer        chan string
	viewerBytes   atomic.Int64
	viewerDropped atomic.Uint64
	queuedBytes   atomic.Int64
	dropped       atomic.Uint64
	spool         *delivery.Spool

	// Only the persistence goroutine owns these fields. Bytes remain charged to
	// queuedBytes until the exact batch identity has been published on disk.
	pendingPayload []byte
	pendingBytes   int64
	carryLog       *pb.HTTPAccessLog
	sendBatch      func(context.Context, *pb.CreateHTTPAccessLogsRequest) error
}

// NewHTTPAccessLogQueue 获取新对象
func NewHTTPAccessLogQueue() *HTTPAccessLogQueue {
	// 队列中最大的值，超出此数量的访问日志会被丢弃
	var maxSize = 2_000 * (1 + memutils.SystemMemoryGB()/2)
	if maxSize > 20_000 {
		maxSize = 20_000
	}

	var queue = &HTTPAccessLogQueue{
		queue:  make(chan *pb.HTTPAccessLog, maxSize),
		wake:   make(chan struct{}, 1),
		flush:  make(chan accessLogFlushRequest),
		viewer: make(chan string, 128),
		spool:  delivery.NewSpool(filepath.Join(Tea.Root, "data", "spool", "access-logs"), 128<<20, 8<<20),
	}
	goman.New(func() {
		queue.Start()
	})

	return queue
}

// Start 开始处理访问日志
const accessLogBatchBytes = 4 << 20
const accessLogQueueBytes = 64 << 20

func (this *HTTPAccessLogQueue) Start() {
	flushOnExit := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := this.FlushToDisk(ctx); err != nil {
			remotelogs.Error("ACCESS_LOG_SHUTDOWN", "could not persist all admitted logs before exit: "+err.Error())
		}
	}
	events.OnKey(events.EventQuit, this, flushOnExit)
	// SIGTERM reaches EventTerminated directly. A graceful quit can also admit
	// final in-flight request logs after its initial EventQuit notification.
	events.OnKey(events.EventTerminated, this, flushOnExit)
	// Persistence must continue independently while an earlier RPC is stalled.
	// Otherwise an API outage leaves later logs in RAM despite free spool space.
	goman.New(func() { this.sendLoop() })
	goman.New(func() {
		for line := range this.viewer {
			sharedHTTPAccessLogViewer.sendLine(line)
			this.viewerBytes.Add(-int64(len(line)))
		}
	})
	this.persistLoop(context.Background())
}

func (this *HTTPAccessLogQueue) persistLoop(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-this.flush:
			request.done <- this.flushToDisk(request.ctx)
			continue
		case <-ticker.C:
		case <-this.wake:
		}
		for batch := 0; batch < 8; batch++ {
			if err := this.persistBatch(); err != nil {
				remotelogs.Error("ACCESS_LOG_SPOOL", err.Error())
				break
			}
			if len(this.queue) == 0 && this.carryLog == nil {
				break
			}
		}
	}
}

// FlushToDisk asks the persistence worker to publish admitted RAM logs, without
// waiting for any RPC. A deadline bounds both worker availability and the wait
// for disk I/O; a timed-out worker may finish its already-started write later.
// Callers should stop producers before the final shutdown flush. Unpublished
// data remains charged in RAM on failure and is not reported as dropped.
func (this *HTTPAccessLogQueue) FlushToDisk(ctx context.Context) error {
	request := accessLogFlushRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case this.flush <- request:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-request.done:
		return err
	}
}

func (this *HTTPAccessLogQueue) flushToDisk(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := this.persistBatch(); err != nil {
			return err
		}
		if len(this.pendingPayload) == 0 && this.carryLog == nil && len(this.queue) == 0 {
			return nil
		}
	}
}

func (this *HTTPAccessLogQueue) sendLoop() {
	retryDelay := 250 * time.Millisecond
	timer := time.NewTimer(retryDelay)
	defer timer.Stop()
	for range timer.C {
		if err := this.loop(); err != nil {
			if rpc.IsConnError(err) {
				remotelogs.Debug("ACCESS_LOG_QUEUE", err.Error())
			} else {
				remotelogs.Error("ACCESS_LOG_QUEUE", err.Error())
			}
			retryDelay *= 2
			if retryDelay > 30*time.Second {
				retryDelay = 30 * time.Second
			}
		} else {
			retryDelay = 250 * time.Millisecond
		}
		timer.Reset(retryDelay)
	}
}

// Dropped counts logs rejected by admission limits or invalid serialization.
func (this *HTTPAccessLogQueue) Dropped() uint64 { return this.dropped.Load() }

func (this *HTTPAccessLogQueue) Quarantined() uint64 { return this.spool.Quarantined() }

func (this *HTTPAccessLogQueue) Push(accessLog *pb.HTTPAccessLog) {
	if accessLog == nil {
		return
	}
	size := int64(proto.Size(accessLog))
	if size > accessLogBatchBytes-8 || this.queuedBytes.Add(size) > accessLogQueueBytes {
		if size <= accessLogBatchBytes-8 {
			this.queuedBytes.Add(-size)
		}
		this.dropped.Add(1)
		return
	}
	select {
	case this.queue <- accessLog:
		if len(this.queue) >= 1000 {
			select {
			case this.wake <- struct{}{}:
			default:
			}
		}
	default:
		this.queuedBytes.Add(-size)
		this.dropped.Add(1)
	}
}

// persistBatch retains admitted logs in bounded memory when disk is full. An
// ambiguous post-rename error never regenerates an ID or counts logs as dropped.
func (this *HTTPAccessLogQueue) persistBatch() error {
	if len(this.pendingPayload) == 0 {
		var logs []*pb.HTTPAccessLog
		var size int
		var charged int64
	Drain:
		for len(logs) < 2000 {
			var log *pb.HTTPAccessLog
			if this.carryLog != nil {
				log = this.carryLog
				this.carryLog = nil
			} else {
				select {
				case log = <-this.queue:
				default:
					break Drain
				}
			}
			entrySize := proto.Size(log)
			// Each repeated message also carries a tag and length prefix.
			if len(logs) > 0 && size+entrySize+8 > accessLogBatchBytes {
				this.carryLog = log
				break
			}
			this.ToValidUTF8(log)
			this.showLocalLog(log)
			logs = append(logs, log)
			size += entrySize + 8
			charged += int64(entrySize)
		}
		if len(logs) == 0 {
			return nil
		}
		payload, err := proto.Marshal(&pb.CreateHTTPAccessLogsRequest{HttpAccessLogs: logs})
		if err != nil {
			this.queuedBytes.Add(-charged)
			this.dropped.Add(uint64(len(logs)))
			return err
		}
		this.pendingPayload = payload
		this.pendingBytes = charged
	}
	id, err := this.spool.Append(this.pendingPayload)
	if id != "" {
		this.queuedBytes.Add(-this.pendingBytes)
		this.pendingPayload = nil
		this.pendingBytes = 0
	}
	return err
}

// The optional local console viewer is best effort and has an independent
// 1 MiB budget. A slow console must never block authoritative disk persistence.
func (this *HTTPAccessLogQueue) showLocalLog(log *pb.HTTPAccessLog) {
	if this.viewer == nil || !sharedHTTPAccessLogViewer.HasConns() {
		return
	}
	line := formatAccessLogLine(log)
	size := int64(len(line))
	if this.viewerBytes.Add(size) > 1<<20 {
		this.viewerBytes.Add(-size)
		this.viewerDropped.Add(1)
		return
	}
	select {
	case this.viewer <- line:
	default:
		this.viewerBytes.Add(-size)
		this.viewerDropped.Add(1)
	}
}

// ViewerDropped counts best-effort local console frames, separately from logs
// rejected by authoritative admission and without changing spool accounting.
func (this *HTTPAccessLogQueue) ViewerDropped() uint64 { return this.viewerDropped.Load() }

// loop sends only already-published batches. Its network deadline cannot stop
// the independent producer from draining more logs into the disk budget.
func (this *HTTPAccessLogQueue) loop() error {
	batches, err := this.spool.Pending()
	if err != nil || len(batches) == 0 {
		return err
	}

	if this.rpcClient == nil && this.sendBatch == nil {
		client, err := rpc.SharedRPC()
		if err != nil {
			return err
		}
		this.rpcClient = client
	}
	for _, batch := range batches {
		var request pb.CreateHTTPAccessLogsRequest
		if err := proto.Unmarshal(batch.Data, &request); err != nil {
			if quarantineErr := this.spool.Quarantine(batch.ID, "invalid protobuf payload: "+err.Error()); quarantineErr != nil {
				return errors.Join(err, quarantineErr)
			}
			remotelogs.Error("ACCESS_LOG_QUARANTINE", "isolated batch "+batch.ID+": "+err.Error())
			continue
		}
		base := context.Background()
		if this.rpcClient != nil {
			base = this.rpcClient.Context()
		}
		ctx, cancel := context.WithTimeout(base, 10*time.Second)
		ctx = metadata.AppendToOutgoingContext(ctx, "x-edge-access-log-batch", batch.ID)
		if this.sendBatch != nil {
			err = this.sendBatch(ctx, &request)
		} else {
			_, err = this.rpcClient.HTTPAccessLogRPC.CreateHTTPAccessLogs(ctx, &request)
		}
		cancel()
		if err != nil {
			switch status.Code(err) {
			case codes.InvalidArgument, codes.PermissionDenied, codes.DataLoss:
				if quarantineErr := this.spool.Quarantine(batch.ID, err.Error()); quarantineErr != nil {
					return errors.Join(err, quarantineErr)
				}
				remotelogs.Error("ACCESS_LOG_QUARANTINE", "isolated batch "+batch.ID+": "+err.Error())
				continue
			default:
				return err
			}
		}
		if err := this.spool.Ack(batch.ID); err != nil {
			return err
		}
	}
	return nil
}

// ToValidUTF8 处理访问日志中的非UTF-8字节
func (this *HTTPAccessLogQueue) ToValidUTF8(accessLog *pb.HTTPAccessLog) {
	accessLog.RemoteAddr = utils.ToValidUTF8string(accessLog.RemoteAddr)
	accessLog.RemoteUser = utils.ToValidUTF8string(accessLog.RemoteUser)
	accessLog.RequestURI = utils.ToValidUTF8string(accessLog.RequestURI)
	accessLog.RequestPath = utils.ToValidUTF8string(accessLog.RequestPath)
	accessLog.RequestFilename = utils.ToValidUTF8string(accessLog.RequestFilename)
	accessLog.RequestBody = bytes.ToValidUTF8(accessLog.RequestBody, []byte{})
	accessLog.Host = utils.ToValidUTF8string(accessLog.Host)
	accessLog.Hostname = utils.ToValidUTF8string(accessLog.Hostname)

	for k, v := range accessLog.SentHeader {
		if !utf8.ValidString(k) {
			delete(accessLog.SentHeader, k)
			continue
		}

		for index, s := range v.Values {
			v.Values[index] = utils.ToValidUTF8string(s)
		}
	}

	accessLog.Referer = utils.ToValidUTF8string(accessLog.Referer)
	accessLog.UserAgent = utils.ToValidUTF8string(accessLog.UserAgent)
	accessLog.Request = utils.ToValidUTF8string(accessLog.Request)
	accessLog.ContentType = utils.ToValidUTF8string(accessLog.ContentType)

	for k, c := range accessLog.Cookie {
		if !utf8.ValidString(k) {
			delete(accessLog.Cookie, k)
			continue
		}
		accessLog.Cookie[k] = utils.ToValidUTF8string(c)
	}

	accessLog.Args = utils.ToValidUTF8string(accessLog.Args)
	accessLog.QueryString = utils.ToValidUTF8string(accessLog.QueryString)

	for k, v := range accessLog.Header {
		if !utf8.ValidString(k) {
			delete(accessLog.Header, k)
			continue
		}
		for index, s := range v.Values {
			v.Values[index] = utils.ToValidUTF8string(s)
		}
	}

	for k, v := range accessLog.Errors {
		accessLog.Errors[k] = utils.ToValidUTF8string(v)
	}
}
