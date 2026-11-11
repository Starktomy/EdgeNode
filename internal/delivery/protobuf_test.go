package delivery

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func wireMessage(number protowire.Number, payload []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), payload)
}

func wireGroup(depth int) []byte {
	var data []byte
	for i := 0; i < depth; i++ {
		data = protowire.AppendTag(data, protowire.Number(100+i), protowire.StartGroupType)
	}
	for i := depth - 1; i >= 0; i-- {
		data = protowire.AppendTag(data, protowire.Number(100+i), protowire.EndGroupType)
	}
	return data
}

func TestValidateBatchRecordsBoundaryAndMixedTrafficFields(t *testing.T) {
	var logs pb.CreateHTTPAccessLogsRequest
	valid := bytes.Repeat(wireMessage(1, nil), MaxBatchRecords)
	if err := ValidateBatchRecords(valid, logs.ProtoReflect().Descriptor(), 1); err != nil {
		t.Fatal("normal log transaction rejected", err)
	}
	if err := ValidateBatchRecords(append(valid, wireMessage(1, nil)...), logs.ProtoReflect().Descriptor(), 1); !errors.Is(err, ErrBatchRecords) {
		t.Fatal("tiny repeated logs bypassed the record cap", err)
	}
	var traffic pb.UploadServerDailyStatsRequest
	valid = append(bytes.Repeat(wireMessage(1, nil), MaxBatchRecords/2), bytes.Repeat(wireMessage(2, nil), MaxBatchRecords/2)...)
	if err := ValidateBatchRecords(valid, traffic.ProtoReflect().Descriptor(), 1, 2); err != nil {
		t.Fatal("mixed legacy traffic transaction rejected", err)
	}
	if err := ValidateBatchRecords(append(valid, wireMessage(2, nil)...), traffic.ProtoReflect().Descriptor(), 1, 2); !errors.Is(err, ErrBatchRecords) {
		t.Fatal("separate traffic fields bypassed the combined record cap", err)
	}
}

func TestValidateBatchNestedRepeatedBudgets(t *testing.T) {
	var logs pb.CreateHTTPAccessLogsRequest
	descriptor := logs.ProtoReflect().Descriptor()
	for _, number := range []protowire.Number{42, 49, 50} { // errors, firewallActions, tags
		valid := wireMessage(1, bytes.Repeat(wireMessage(number, nil), MaxBatchElements-1))
		if err := ValidateBatchRecords(valid, descriptor, 1); err != nil {
			t.Fatal("nested string boundary rejected", number, err)
		}
		invalid := wireMessage(1, bytes.Repeat(wireMessage(number, nil), MaxBatchElements))
		if err := ValidateBatchRecords(invalid, descriptor, 1); !errors.Is(err, ErrBatchElements) {
			t.Fatal("nested strings bypassed the aggregate budget", number, err)
		}
	}
	// HTTPAccessLog.header -> map entry.value -> Strings.values.
	values := bytes.Repeat(wireMessage(1, nil), MaxBatchElements-2)
	valid := wireMessage(1, wireMessage(36, wireMessage(2, values)))
	if err := ValidateBatchRecords(valid, descriptor, 1); err != nil {
		t.Fatal("map string boundary rejected", err)
	}
	values = append(values, wireMessage(1, nil)...)
	invalid := wireMessage(1, wireMessage(36, wireMessage(2, values)))
	if err := ValidateBatchRecords(invalid, descriptor, 1); !errors.Is(err, ErrBatchElements) {
		t.Fatal("map value strings bypassed the aggregate budget", err)
	}
	// Node.connectedAPINodeIds uses packed varints and can amplify inside a
	// single non-repeated Node field. The outer access log consumes one element.
	valid = wireMessage(1, wireMessage(100, wireMessage(11, make([]byte, MaxBatchElements-1))))
	if err := ValidateBatchRecords(valid, descriptor, 1); err != nil {
		t.Fatal("packed integer boundary rejected", err)
	}
	invalid = wireMessage(1, wireMessage(100, wireMessage(11, make([]byte, MaxBatchElements))))
	if err := ValidateBatchRecords(invalid, descriptor, 1); !errors.Is(err, ErrBatchElements) {
		t.Fatal("packed nested values bypassed the aggregate budget", err)
	}
}

func TestValidateBatchUnknownFieldsRemainOpaque(t *testing.T) {
	var request pb.CreateHTTPAccessLogsRequest
	data := wireMessage(1, nil)
	// Unknown bytes need not encode a message. An unknown group containing
	// tags identical to the record field must not count as top-level records.
	data = append(data, wireMessage(99, []byte{0xff, 0xff, 0xff})...)
	data = protowire.AppendTag(data, 98, protowire.StartGroupType)
	data = append(data, bytes.Repeat(wireMessage(1, nil), MaxBatchRecords+1)...)
	data = protowire.AppendTag(data, 98, protowire.EndGroupType)
	if err := ValidateBatchRecords(data, request.ProtoReflect().Descriptor(), 1); err != nil {
		t.Fatal("forward-compatible unknown fields were rejected", err)
	}
	if err := (proto.UnmarshalOptions{RecursionLimit: MaxBatchDepth}).Unmarshal(data, &request); err != nil || len(request.HttpAccessLogs) != 1 {
		t.Fatal("preflight accepted incompatible wire", err)
	}
}

func TestValidateBatchRejectsMalformedWireAndDeepGroups(t *testing.T) {
	var request pb.CreateHTTPAccessLogsRequest
	descriptor := request.ProtoReflect().Descriptor()
	malformed := [][]byte{
		{0},                             // field zero
		{0x0a, 0x80},                    // truncated length
		{0x0a, 2, 0},                    // truncated value
		{0x08, 0},                       // wrong wire type for a record
		{0x0a, 1, 0},                    // malformed known inner message
		{0x0e},                          // reserved wire type
		protowire.AppendTag(nil, 99, 4), // orphan end group
		protowire.AppendTag(nil, 99, 3), // unterminated group
		append(protowire.AppendTag(nil, 99, 3), protowire.AppendTag(nil, 100, 4)...),
	}
	for _, data := range malformed {
		if err := ValidateBatchRecords(data, descriptor, 1); !errors.Is(err, ErrBatchWire) {
			t.Fatalf("malformed wire %x accepted or misclassified: %v", data, err)
		}
	}
	if err := ValidateBatchRecords(wireGroup(MaxBatchDepth-1), descriptor, 1); err != nil {
		t.Fatal("bounded unknown groups rejected", err)
	}
	if err := ValidateBatchRecords(wireGroup(MaxBatchDepth), descriptor, 1); !errors.Is(err, ErrBatchDepth) {
		t.Fatal("unknown group recursion was not bounded", err)
	}
	if err := ValidateBatchRecords(wireMessage(1, wireGroup(MaxBatchDepth-1)), descriptor, 1); !errors.Is(err, ErrBatchDepth) {
		t.Fatal("known messages failed to charge the shared depth budget", err)
	}
}

func TestValidateBatchDoesNotAllocatePerRecord(t *testing.T) {
	var request pb.CreateHTTPAccessLogsRequest
	descriptor := request.ProtoReflect().Descriptor()
	valid := bytes.Repeat(wireMessage(1, nil), MaxBatchRecords)
	oversize := append(bytes.Clone(valid), wireMessage(1, nil)...)
	for _, data := range [][]byte{valid, oversize} {
		if allocations := testing.AllocsPerRun(10, func() {
			_ = ValidateBatchRecords(data, descriptor, 1)
		}); allocations != 0 {
			t.Fatalf("preflight allocated per restored batch: %v allocations", allocations)
		}
	}
}

func BenchmarkValidateBatchRecords(b *testing.B) {
	var request pb.CreateHTTPAccessLogsRequest
	descriptor := request.ProtoReflect().Descriptor()
	for _, records := range []int{1000, 2000, 8192} {
		data := bytes.Repeat(wireMessage(1, nil), records)
		b.Run(fmt.Sprintf("Records%d", records), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = ValidateBatchRecords(data, descriptor, 1)
			}
		})
	}
}

func BenchmarkValidateBatchRecordsNested(b *testing.B) {
	log := &pb.HTTPAccessLog{ServerId: 1, RequestId: "test-id", RequestURI: "/assets/main.js", Header: make(map[string]*pb.Strings)}
	for i := 0; i < 20; i++ {
		log.Header[fmt.Sprintf("X-Test-%d", i)] = &pb.Strings{Values: []string{"example-value"}}
	}
	request := &pb.CreateHTTPAccessLogsRequest{}
	for i := 0; i < MaxBatchRecords; i++ {
		request.HttpAccessLogs = append(request.HttpAccessLogs, log)
	}
	data, err := proto.Marshal(request)
	if err != nil {
		b.Fatal(err)
	}
	descriptor := request.ProtoReflect().Descriptor()
	if err := ValidateBatchRecords(data, descriptor, 1); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateBatchRecords(data, descriptor, 1); err != nil {
			b.Fatal(err)
		}
	}
}
