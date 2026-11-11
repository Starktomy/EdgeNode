package delivery

import (
	"bytes"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func FuzzValidateBatchRecords(f *testing.F) {
	logs, err := proto.Marshal(&pb.CreateHTTPAccessLogsRequest{HttpAccessLogs: []*pb.HTTPAccessLog{{
		RequestId: "example", Header: map[string]*pb.Strings{"Accept": {Values: []string{"text/html"}}},
		Tags: []string{"cached"}, Node: &pb.Node{Id: 1, ConnectedAPINodeIds: []int64{1, 2}},
	}}})
	if err != nil {
		f.Fatal(err)
	}
	traffic, err := proto.Marshal(&pb.UploadServerDailyStatsRequest{
		Stats:       []*pb.ServerDailyStat{{ServerId: 1, Bytes: 42}},
		DomainStats: []*pb.UploadServerDailyStatsRequest_DomainStat{{ServerId: 1, Domain: "cdn.example.test", Bytes: 42}},
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(uint8(0), logs)
	f.Add(uint8(1), traffic)
	f.Add(uint8(0), bytes.Repeat(wireMessage(1, nil), MaxBatchRecords+1))
	f.Add(uint8(1), wireGroup(MaxBatchDepth))
	f.Add(uint8(0), wireMessage(99, []byte{0xff, 0xff}))
	f.Add(uint8(0), []byte{0x0a, 0x80})
	f.Add(uint8(1), []byte{0})
	f.Fuzz(func(t *testing.T, kind uint8, data []byte) {
		// Keep arbitrary inputs and reference decoder allocations bounded. The
		// validator's stricter budgets may reject otherwise valid protobuf.
		if len(data) > 32<<10 {
			t.Skip()
		}
		var request proto.Message = &pb.CreateHTTPAccessLogsRequest{}
		fields := []protowire.Number{1}
		if kind%2 == 1 {
			request = &pb.UploadServerDailyStatsRequest{}
			fields = append(fields, 2)
		}
		descriptor := request.ProtoReflect().Descriptor()
		if err := ValidateBatchRecords(data, descriptor, fields...); err != nil {
			return
		}
		// The standard decoder can additionally reject invalid UTF-8 or a known
		// field's wrong wire type. Successful decodes must remain within budgets
		// after canonical re-encoding, including merged messages and maps.
		if err := (proto.UnmarshalOptions{RecursionLimit: MaxBatchDepth}).Unmarshal(data, request); err != nil {
			return
		}
		canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateBatchRecords(canonical, descriptor, fields...); err != nil {
			t.Fatalf("accepted decoded data exceeded budgets after canonical encoding: %v", err)
		}
	})
}
