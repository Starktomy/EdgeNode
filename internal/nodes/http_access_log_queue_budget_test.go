package nodes

import (
	"context"
	"fmt"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestAccessLogProducerSplitsElementBudgetWithoutQuarantiningAcceptedLogs(t *testing.T) {
	queue := deliveryQueue(t, 4<<20)
	queue.queue = make(chan *pb.HTTPAccessLog, 2000)
	headers := make(map[string]*pb.Strings)
	for i := 0; i < 33; i++ {
		headers[fmt.Sprintf("X-Test-%d", i)] = &pb.Strings{Values: []string{"value"}}
	}
	for i := 0; i < 2000; i++ {
		queue.Push(&pb.HTTPAccessLog{RequestId: fmt.Sprintf("request-%d", i), Header: headers})
	}
	if queue.Dropped() != 0 || len(queue.queue) != 2000 {
		t.Fatal("ordinary header-rich logs were rejected during admission")
	}
	for len(queue.queue) > 0 || queue.carryLog != nil || len(queue.pendingPayload) > 0 {
		if err := queue.persistBatch(); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("accepted logs were not split by element budget: %d batches, %v", len(pending), err)
	}
	var descriptor pb.CreateHTTPAccessLogsRequest
	for _, batch := range pending {
		if err := delivery.ValidateBatchRecords(batch.Data, descriptor.ProtoReflect().Descriptor(), 1); err != nil {
			t.Fatal("producer generated a batch its sender rejects", err)
		}
	}
	seen := make(map[string]bool)
	queue.sendBatch = func(_ context.Context, request *pb.CreateHTTPAccessLogsRequest) error {
		for _, log := range request.HttpAccessLogs {
			if seen[log.RequestId] {
				t.Fatal("element-budget carry duplicated an accepted log")
			}
			seen[log.RequestId] = true
		}
		return nil
	}
	if err := queue.loop(); err != nil || len(seen) != 2000 || queue.Quarantined() != 0 || queue.Dropped() != 0 || queue.queuedBytes.Load() != 0 {
		t.Fatalf("accepted logs did not drain intact: seen=%d quarantined=%d dropped=%d charged=%d err=%v", len(seen), queue.Quarantined(), queue.Dropped(), queue.queuedBytes.Load(), err)
	}
}

func TestAccessLogSingleElementBudgetBoundaryRejectsBeforeAdmission(t *testing.T) {
	queue := deliveryQueue(t, 4<<20)
	queue.Push(&pb.HTTPAccessLog{RequestId: "boundary", Tags: make([]string, delivery.MaxBatchElements-1)})
	queue.Push(&pb.HTTPAccessLog{RequestId: "oversize", Tags: make([]string, delivery.MaxBatchElements)})
	if queue.Dropped() != 1 || len(queue.queue) != 1 {
		t.Fatal("single-log budget did not distinguish accepted and rejected records")
	}
	if err := queue.persistBatch(); err != nil || queue.queuedBytes.Load() != 0 {
		t.Fatal("accepted boundary log could not persist", err)
	}
	var sent int
	queue.sendBatch = func(_ context.Context, request *pb.CreateHTTPAccessLogsRequest) error {
		if len(request.HttpAccessLogs) != 1 || request.HttpAccessLogs[0].RequestId != "boundary" {
			t.Fatal("rejected wide log entered a published batch")
		}
		sent++
		return nil
	}
	if err := queue.loop(); err != nil || sent != 1 || queue.Quarantined() != 0 {
		t.Fatal("accepted boundary log was quarantined", err)
	}
}

func TestAccessLogObjectElementCountMatchesSchemaWire(t *testing.T) {
	unknownValues := &pb.Strings{Values: []string{"normal"}}
	unknownValues.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), nil))
	unknownLog := &pb.HTTPAccessLog{}
	unknownLog.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 50, protowire.BytesType), nil))
	cases := []struct {
		log  *pb.HTTPAccessLog
		want int
	}{
		{&pb.HTTPAccessLog{}, 1},
		{&pb.HTTPAccessLog{Header: map[string]*pb.Strings{"nil": nil}}, 2},
		{&pb.HTTPAccessLog{Header: map[string]*pb.Strings{"X-Test": unknownValues}}, 4},
		{unknownLog, 2},
		{&pb.HTTPAccessLog{Node: &pb.Node{ConnectedAPINodeIds: []int64{1, 2}}, Header: map[string]*pb.Strings{"nil": nil}}, 4},
	}
	for _, item := range cases {
		if got, err := accessLogElementCount(item.log); err != nil || got != item.want {
			t.Fatalf("object/wire element accounting differs: got=%d want=%d err=%v", got, item.want, err)
		}
		request := &pb.CreateHTTPAccessLogsRequest{HttpAccessLogs: []*pb.HTTPAccessLog{item.log}}
		data, err := proto.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := delivery.ValidateBatchRecords(data, request.ProtoReflect().Descriptor(), 1); err != nil {
			t.Fatal("admitted object generated invalid wire", err)
		}
	}
	wideNode := &pb.HTTPAccessLog{Node: &pb.Node{ConnectedAPINodeIds: make([]int64, delivery.MaxBatchElements)}}
	if _, err := accessLogElementCount(wideNode); err == nil {
		t.Fatal("nested Node values bypassed admission budget")
	}
	malformed := &pb.HTTPAccessLog{}
	malformed.ProtoReflect().SetUnknown([]byte{0})
	queue := deliveryQueue(t, 4<<20)
	queue.Push(wideNode)
	queue.Push(malformed)
	if queue.Dropped() != 2 || len(queue.queue) != 0 || queue.queuedBytes.Load() != 0 {
		t.Fatal("invalid nested/unknown objects were accepted or charged")
	}
}

func TestAccessLogChangedAdmittedObjectRetainsEarlierBatchAndCarry(t *testing.T) {
	queue := deliveryQueue(t, 4<<20)
	first := &pb.HTTPAccessLog{RequestId: "first"}
	changed := &pb.HTTPAccessLog{RequestId: "changed"}
	queue.Push(first)
	queue.Push(changed)
	charged := queue.queuedBytes.Load() - int64(proto.Size(first))
	// Callers must leave admitted logs immutable. If this invariant is broken,
	// the new budget check must still preserve earlier records and the carry.
	changed.ProtoReflect().SetUnknown([]byte{0})
	if err := queue.persistBatch(); err != nil {
		t.Fatal("a later invalid object prevented the earlier batch from publishing", err)
	}
	if queue.carryLog != changed || queue.queuedBytes.Load() != charged || queue.Dropped() != 0 {
		t.Fatal("admitted carry or its RAM charge was lost")
	}
	if err := queue.persistBatch(); err == nil || queue.carryLog != changed {
		t.Fatal("invalid admitted object was hidden or lost")
	}
	pending, err := queue.spool.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatal("earlier accepted batch disappeared", err)
	}
	var request pb.CreateHTTPAccessLogsRequest
	if err := proto.Unmarshal(pending[0].Data, &request); err != nil || len(request.HttpAccessLogs) != 1 || request.HttpAccessLogs[0].RequestId != "first" {
		t.Fatal("the published prefix changed", err)
	}
}

func BenchmarkAccessLogPushBudget(b *testing.B) {
	for _, headers := range []int{0, 20, 40} {
		b.Run(fmt.Sprintf("Headers%d", headers), func(b *testing.B) {
			log := &pb.HTTPAccessLog{RequestId: "benchmark", Header: make(map[string]*pb.Strings)}
			for i := 0; i < headers; i++ {
				log.Header[fmt.Sprintf("X-Test-%d", i)] = &pb.Strings{Values: []string{"value"}}
			}
			queue := &HTTPAccessLogQueue{queue: make(chan *pb.HTTPAccessLog, 1), wake: make(chan struct{}, 1)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				queue.Push(log)
				<-queue.queue
				queue.queuedBytes.Store(0)
			}
		})
	}
}
