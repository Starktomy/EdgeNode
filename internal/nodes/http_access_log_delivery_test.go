package nodes

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func deliveryQueue(t *testing.T, maxBytes int64) *HTTPAccessLogQueue {
	t.Helper()
	return &HTTPAccessLogQueue{queue: make(chan *pb.HTTPAccessLog, 20), wake: make(chan struct{}, 1), flush: make(chan accessLogFlushRequest), spool: delivery.NewSpool(filepath.Join(t.TempDir(), "spool"), maxBytes, 8<<20)}
}

func startLogPersistence(t *testing.T, queue *HTTPAccessLogQueue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		queue.persistLoop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestAccessLogShutdownFlushPublishesRAMWithoutRPC(t *testing.T) {
	queue := deliveryQueue(t, 16<<20)
	for _, id := range []string{"one", "two"} {
		queue.Push(&pb.HTTPAccessLog{RequestId: id, RequestBody: []byte(strings.Repeat("x", 3<<20))})
	}
	queue.sendBatch = func(context.Context, *pb.CreateHTTPAccessLogsRequest) error {
		t.Error("shutdown flush attempted an RPC")
		return errors.New("RPC must not be required")
	}
	startLogPersistence(t, queue)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := queue.FlushToDisk(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 2 || queue.queuedBytes.Load() != 0 || queue.Dropped() != 0 {
		t.Fatal("shutdown did not persist all bounded batches", err)
	}
	seen := map[string]bool{}
	for _, batch := range pending {
		var request pb.CreateHTTPAccessLogsRequest
		if err := proto.Unmarshal(batch.Data, &request); err != nil {
			t.Fatal(err)
		}
		for _, log := range request.HttpAccessLogs {
			seen[log.RequestId] = true
		}
	}
	if !seen["one"] || !seen["two"] {
		t.Fatal("shutdown lost an admitted RAM log")
	}
}

func TestAccessLogLocalViewerPreservedWithoutBlockingPersistence(t *testing.T) {
	viewerConn, client := net.Pipe()
	defer client.Close()
	defer viewerConn.Close()
	viewer := sharedHTTPAccessLogViewer
	viewer.locker.Lock()
	id := viewer.nextConnId()
	viewer.connMap[id] = viewerConn
	viewer.locker.Unlock()
	t.Cleanup(func() {
		viewer.locker.Lock()
		delete(viewer.connMap, id)
		viewer.locker.Unlock()
	})
	queue := deliveryQueue(t, 1<<20)
	queue.viewer = make(chan string, 1)
	queue.Push(&pb.HTTPAccessLog{RequestId: "first", RequestMethod: "GET", Scheme: "https", Host: "example.test", RequestURI: "/first"})
	if err := queue.persistBatch(); err != nil {
		t.Fatal(err)
	}
	queue.Push(&pb.HTTPAccessLog{RequestId: "second", RequestURI: "/second"})
	if err := queue.persistBatch(); err != nil {
		t.Fatal("unconsumed local viewer blocked persistence", err)
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 2 || queue.Dropped() != 0 || queue.ViewerDropped() != 1 {
		t.Fatal("viewer backpressure changed authoritative log delivery", err)
	}
	if line := <-queue.viewer; !strings.Contains(line, "GET https://example.test/first") {
		t.Fatal("local access-log output was lost", line)
	}
	queue.viewerBytes.Store(0)
	queue.showLocalLog(&pb.HTTPAccessLog{RequestURI: strings.Repeat("x", 2<<20)})
	if queue.ViewerDropped() != 2 || queue.viewerBytes.Load() != 0 || len(queue.viewer) != 0 {
		t.Fatal("local viewer byte budget was not enforced")
	}
}

func TestAccessLogSlowLocalViewerHasWriteDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	viewer := NewHTTPAccessLogViewer()
	viewer.connMap[1] = server
	started := time.Now()
	viewer.sendLine("an unread local console line")
	if time.Since(started) > time.Second {
		t.Fatal("slow console blocked the viewer worker")
	}
	var one [1]byte
	if _, err := client.Read(one[:]); !errors.Is(err, io.EOF) {
		t.Fatal("slow console was not disconnected", err)
	}
}

func TestAccessLogShutdownFlushReportsFullDiskWithoutDropping(t *testing.T) {
	log := &pb.HTTPAccessLog{RequestId: "first"}
	payload, err := proto.Marshal(&pb.CreateHTTPAccessLogsRequest{HttpAccessLogs: []*pb.HTTPAccessLog{log}})
	if err != nil {
		t.Fatal(err)
	}
	queue := deliveryQueue(t, int64(len(payload)))
	queue.Push(log)
	if err := queue.persistBatch(); err != nil {
		t.Fatal(err)
	}
	queue.Push(&pb.HTTPAccessLog{RequestId: "other"})
	startLogPersistence(t, queue)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := queue.FlushToDisk(ctx); !errors.Is(err, delivery.ErrFull) {
		t.Fatal("shutdown hid a full disk", err)
	}
	if queue.queuedBytes.Load() == 0 || queue.Dropped() != 0 {
		t.Fatal("failed shutdown flush changed unpersisted admission accounting")
	}
}

func TestAccessLogShutdownFlushDeadlineBoundsUnavailableOrBusyWorker(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "busy"}[busy], func(t *testing.T) {
			queue := deliveryQueue(t, 1<<20)
			if busy {
				release := make(chan struct{})
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					select {
					case request := <-queue.flush:
						<-release // Simulate a filesystem operation that ignores context.
						request.done <- nil
					case <-release:
					}
				}()
				t.Cleanup(func() {
					close(release)
					<-finished
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			started := time.Now()
			if err := queue.FlushToDisk(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("flush ignored the caller deadline", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("flush hung shutdown")
			}
		})
	}
}

func TestAccessLogProducerPersistsWhileEarlierBatchRetries(t *testing.T) {
	queue := deliveryQueue(t, 1<<20)
	queue.Push(&pb.HTTPAccessLog{RequestId: "first", NodeId: 1})
	if err := queue.persistBatch(); err != nil {
		t.Fatal(err)
	}
	queue.sendBatch = func(context.Context, *pb.CreateHTTPAccessLogsRequest) error { return errors.New("API offline") }
	if err := queue.loop(); err == nil {
		t.Fatal("expected transport failure")
	}
	queue.Push(&pb.HTTPAccessLog{RequestId: "second", NodeId: 1})
	if err := queue.persistBatch(); err != nil {
		t.Fatal("pending transport blocked persistence", err)
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("later logs were not durable: %d %v", len(pending), err)
	}
	if queue.queuedBytes.Load() != 0 || queue.Dropped() != 0 {
		t.Fatal("published logs still charged or dropped")
	}
}

func TestAccessLogFullSpoolRetainsMemoryAndRecovers(t *testing.T) {
	log := &pb.HTTPAccessLog{RequestId: "first", NodeId: 1}
	payload, err := proto.Marshal(&pb.CreateHTTPAccessLogsRequest{HttpAccessLogs: []*pb.HTTPAccessLog{log}})
	if err != nil {
		t.Fatal(err)
	}
	queue := deliveryQueue(t, int64(len(payload)))
	queue.Push(log)
	if err := queue.persistBatch(); err != nil {
		t.Fatal(err)
	}
	queue.Push(&pb.HTTPAccessLog{RequestId: "other", NodeId: 1})
	if err := queue.persistBatch(); !errors.Is(err, delivery.ErrFull) {
		t.Fatal("expected full spool", err)
	}
	if len(queue.pendingPayload) == 0 || queue.queuedBytes.Load() == 0 || queue.Dropped() != 0 {
		t.Fatal("disk-full discarded admitted logs")
	}
	sent := map[string]int{}
	queue.sendBatch = func(ctx context.Context, req *pb.CreateHTTPAccessLogsRequest) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		if len(md.Get("x-edge-access-log-batch")) != 1 {
			t.Fatal("retry identity not sent")
		}
		sent[req.HttpAccessLogs[0].RequestId]++
		return nil
	}
	if err := queue.loop(); err != nil {
		t.Fatal("full spool could not drain", err)
	}
	if err := queue.persistBatch(); err != nil {
		t.Fatal("retained logs did not persist after drain", err)
	}
	if err := queue.loop(); err != nil {
		t.Fatal(err)
	}
	if sent["first"] != 1 || sent["other"] != 1 || queue.queuedBytes.Load() != 0 {
		t.Fatal("recovery lost or duplicated locally admitted logs", sent)
	}
}

func TestAccessLogBatchCarryKeepsPublishedSizeBounded(t *testing.T) {
	queue := deliveryQueue(t, 16<<20)
	for _, id := range []string{"one", "two"} {
		queue.Push(&pb.HTTPAccessLog{RequestId: id, RequestBody: []byte(strings.Repeat("x", 3<<20))})
	}
	if err := queue.persistBatch(); err != nil {
		t.Fatal(err)
	}
	if queue.carryLog == nil || queue.queuedBytes.Load() == 0 {
		t.Fatal("large next log did not remain accounted for")
	}
	if err := queue.persistBatch(); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatal("large logs not split", err)
	}
	for _, batch := range pending {
		if len(batch.Data) > accessLogBatchBytes {
			t.Fatal("batch exceeded the declared memory/network budget")
		}
	}
}

func TestPermanentAccessLogFailureIsQuarantinedAndNextBatchSends(t *testing.T) {
	queue := deliveryQueue(t, 1<<20)
	for _, id := range []string{"poison", "good"} {
		queue.Push(&pb.HTTPAccessLog{RequestId: id, NodeId: 1})
		if err := queue.persistBatch(); err != nil {
			t.Fatal(err)
		}
	}
	sent := 0
	queue.sendBatch = func(_ context.Context, req *pb.CreateHTTPAccessLogsRequest) error {
		if req.HttpAccessLogs[0].RequestId == "poison" {
			return status.Error(codes.InvalidArgument, "permanent validation failure")
		}
		sent++
		return nil
	}
	if err := queue.loop(); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || queue.Quarantined() != 1 {
		t.Fatal("poison batch blocked valid logs or was silently dropped")
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatal("sender did not progress", err)
	}
}
