package stats

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestSecurityTrafficWildcardDomainsCannotGrowWithoutBound(t *testing.T) {
	manager := NewTrafficStatManager()
	for i := 0; i < 1000; i++ {
		manager.Add(42, 9, fmt.Sprintf("host-%d.example.test", i), 1, 0, 1, 0, 0, 0, 0, false, 0)
	}
	if len(manager.domainsMap[9]) != maxBufferedDomainsPerServer || manager.DroppedDomainSamples() != 1000-maxBufferedDomainsPerServer {
		t.Fatal("wildcard domain breakdown exceeded its sampling budget")
	}
	var total int64
	for _, item := range manager.itemMap {
		total += item.Bytes
	}
	if total != 1000 {
		t.Fatalf("sampling domain breakdown changed billable server totals: %d", total)
	}
}

func TestSecurityTrafficFailureMergePreservesDomainBudgetAndServerTotals(t *testing.T) {
	manager := NewTrafficStatManager()
	for i := 0; i < maxBufferedDomainsPerServer; i++ {
		manager.Add(42, 9, fmt.Sprintf("old-%d.example.test", i), 1, 0, 1, 0, 0, 0, 0, false, 0)
	}
	payload, _ := proto.Marshal(&pb.UploadServerDailyStatsRequest{Stats: []*pb.ServerDailyStat{{ServerId: 9, Bytes: 1}}})
	manager.batchSpool = delivery.NewSpool(t.TempDir(), int64(len(payload)), 1<<20)
	if _, err := manager.batchSpool.Append(payload); err != nil {
		t.Fatal(err)
	}
	var attempts int
	manager.sendBatch = func(context.Context, string, *pb.UploadServerDailyStatsRequest) error {
		attempts++
		if attempts == 2 {
			// New live requests arrive while the failed snapshot waits for space.
			for i := 0; i < maxBufferedDomainsPerServer; i++ {
				manager.Add(42, 9, fmt.Sprintf("new-%d.example.test", i), 1, 0, 1, 0, 0, 0, 0, false, 0)
			}
		}
		return status.Error(codes.Unavailable, "API offline")
	}
	if err := manager.Upload(); err == nil {
		t.Fatal("full spool was hidden")
	}
	if attempts != 2 || len(manager.domainsMap[9]) > maxBufferedDomainsPerServer {
		t.Fatal("failed snapshot restoration exceeded the domain sampling budget")
	}
	var total int64
	for _, item := range manager.itemMap {
		total += item.Bytes
	}
	if total != 2*maxBufferedDomainsPerServer {
		t.Fatalf("sampling restoration changed server totals: %d", total)
	}
}

func TestSecurityTrafficSpoolFullStillDrains(t *testing.T) {
	spool := delivery.NewSpool(t.TempDir(), 256, 256)
	payload, _ := proto.Marshal(&pb.UploadServerDailyStatsRequest{Stats: []*pb.ServerDailyStat{{ServerId: 1, Bytes: 42}}})
	var admitted int
	for {
		if _, err := spool.Append(payload); err != nil {
			if !errors.Is(err, delivery.ErrFull) {
				t.Fatal(err)
			}
			break
		}
		admitted++
	}
	manager := NewTrafficStatManager()
	manager.batchSpool = spool
	manager.itemMap[trafficBucketKey{timestamp: time.Now().Unix() / 300 * 300, serverID: 1}] = &TrafficItem{Bytes: 7, CountRequests: 1}
	var delivered int64
	manager.sendBatch = func(_ context.Context, _ string, req *pb.UploadServerDailyStatsRequest) error {
		for _, stat := range req.Stats {
			delivered += stat.Bytes
		}
		return nil
	}
	for i := 0; i < admitted+2; i++ {
		if err := manager.Upload(); err != nil && !errors.Is(err, delivery.ErrFull) {
			t.Fatal(err)
		}
	}
	if delivered != int64(admitted)*42+7 || len(manager.itemMap) != 0 {
		t.Fatalf("full spool stranded or duplicated traffic: delivered=%d entries=%d", delivered, len(manager.itemMap))
	}
}

func TestSecurityTrafficPartialAdmissionRestoresOnlyUnpublishedDeltas(t *testing.T) {
	now := time.Now().Unix() / 300 * 300
	prototype := &pb.UploadServerDailyStatsRequest{}
	for i := 0; i < 1000; i++ {
		prototype.Stats = append(prototype.Stats, &pb.ServerDailyStat{ServerId: 1, Bytes: 1, CountRequests: 1, CreatedAt: now})
	}
	manager := NewTrafficStatManager()
	manager.batchSpool = delivery.NewSpool(t.TempDir(), int64(proto.Size(prototype))+100, 16<<20)
	for i := 0; i < 1500; i++ {
		manager.itemMap[trafficBucketKey{timestamp: now - int64(i)*300, serverID: 1}] = &TrafficItem{Bytes: 1, CountRequests: 1}
	}
	offline := true
	var delivered int64
	manager.sendBatch = func(_ context.Context, _ string, req *pb.UploadServerDailyStatsRequest) error {
		if offline {
			return status.Error(codes.Unavailable, "injected API outage")
		}
		if len(req.Stats)+len(req.DomainStats) > 1000 || proto.Size(req) > 1<<20 {
			t.Fatal("unbounded statistics transaction")
		}
		for _, stat := range req.Stats {
			delivered += stat.Bytes
		}
		return nil
	}
	if err := manager.Upload(); err == nil {
		t.Fatal("outage and full spool were hidden")
	}
	if len(manager.itemMap) != 500 {
		t.Fatalf("restored %d deltas, want only 500 unpublished deltas", len(manager.itemMap))
	}
	offline = false
	if err := manager.Upload(); err != nil {
		t.Fatal(err)
	}
	if delivered != 1500 || len(manager.itemMap) != 0 {
		t.Fatalf("partial retry duplicated/lost totals: %d", delivered)
	}
}

func TestSecurityTrafficRetryPreservesBatchIdentity(t *testing.T) {
	manager := NewTrafficStatManager()
	manager.batchSpool = delivery.NewSpool(t.TempDir(), 1<<20, 1<<20)
	manager.itemMap[trafficBucketKey{timestamp: time.Now().Unix() / 300 * 300, serverID: 1}] = &TrafficItem{Bytes: 19, CountRequests: 1}
	var firstID string
	var successful bool
	manager.sendBatch = func(_ context.Context, id string, _ *pb.UploadServerDailyStatsRequest) error {
		if firstID == "" {
			firstID = id
			return status.Error(codes.Unavailable, "response lost after server commit")
		}
		if id != firstID {
			t.Fatal("retry acquired a different billing identity")
		}
		successful = true
		return nil
	}
	if manager.Upload() == nil {
		t.Fatal("injected response loss was hidden")
	}
	if err := manager.Upload(); err != nil || !successful {
		t.Fatalf("durable retry failed: %v", err)
	}
}

func TestTrafficUploadPreservesStructuredBucketFields(t *testing.T) {
	manager := NewTrafficStatManager()
	manager.batchSpool = delivery.NewSpool(t.TempDir(), 1<<20, 1<<20)
	want := map[trafficBucketKey]int64{
		{timestamp: 300, serverID: 23}:           7,
		{timestamp: 12_000_000_000, serverID: 3}: 11,
	}
	for key, bytes := range want {
		manager.itemMap[key] = &TrafficItem{Bytes: bytes, CountRequests: 1}
	}
	// A delimiter inside a retained domain is data, never part of key parsing.
	const domain = "name@cdn.example.test"
	manager.domainsMap[23] = map[trafficDomainKey]*TrafficItem{
		{timestamp: 300, domain: domain}: {Bytes: 7, CountRequests: 1},
	}
	got := make(map[trafficBucketKey]int64)
	var gotDomain bool
	manager.sendBatch = func(_ context.Context, _ string, req *pb.UploadServerDailyStatsRequest) error {
		for _, stat := range req.Stats {
			got[trafficBucketKey{timestamp: stat.CreatedAt, serverID: stat.ServerId}] += stat.Bytes
		}
		for _, stat := range req.DomainStats {
			gotDomain = stat.ServerId == 23 && stat.CreatedAt == 300 && stat.Domain == domain && stat.Bytes == 7
		}
		return nil
	}
	if err := manager.Upload(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || !gotDomain {
		t.Fatal("structured upload changed bucket identity or domain data", got)
	}
	for key, bytes := range want {
		if got[key] != bytes {
			t.Fatalf("bucket %+v: got %d, want %d", key, got[key], bytes)
		}
	}
	if len(manager.itemMap) != 0 || len(manager.domainsMap) != 0 {
		t.Fatal("committed snapshot left empty retained domain maps")
	}
}

func TestSecurityTrafficRestoredRecordAmplificationIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	manager := NewTrafficStatManager()
	manager.batchSpool = delivery.NewSpool(dir, 1<<20, 1<<20)
	poison := bytes.Repeat([]byte{0x0a, 0}, delivery.MaxBatchRecords+1)
	badID, err := manager.batchSpool.Append(poison)
	if err != nil {
		t.Fatal(err)
	}
	good, err := proto.Marshal(&pb.UploadServerDailyStatsRequest{Stats: []*pb.ServerDailyStat{{ServerId: 1, Bytes: 19}}})
	if err != nil {
		t.Fatal(err)
	}
	goodID, err := manager.batchSpool.Append(good)
	if err != nil {
		t.Fatal(err)
	}
	var sent int
	manager.sendBatch = func(_ context.Context, id string, request *pb.UploadServerDailyStatsRequest) error {
		if id != goodID || len(request.Stats) != 1 || request.Stats[0].Bytes != 19 {
			t.Fatal("amplified restored batch reached the sender")
		}
		sent++
		return nil
	}
	if err := manager.sendPending(); err != nil || sent != 1 || manager.batchSpool.Quarantined() != 1 {
		t.Fatalf("restored amplification blocked valid totals: sent=%d, %v", sent, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "quarantine", badID+".batch")); err != nil || !bytes.Equal(data, poison) {
		t.Fatal("over-budget accounting lost its original identity or payload", err)
	}
	if reason, err := os.ReadFile(filepath.Join(dir, "quarantine", badID+".reason.json")); err != nil || !bytes.Contains(reason, []byte("2000 records")) {
		t.Fatal("decode budget quarantine did not retain an actionable diagnosis", err)
	}
}

func TestSecurityTrafficDecodeBudgetFullQuarantineRetainsOriginalIdentity(t *testing.T) {
	dir := t.TempDir()
	manager := NewTrafficStatManager()
	manager.batchSpool = delivery.NewSpool(dir, 1<<20, 1<<20)
	poison := append(bytes.Repeat([]byte{0x0a, 0}, delivery.MaxBatchRecords), protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), nil)...)
	id, err := manager.batchSpool.Append(poison)
	if err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "quarantine")
	if err := os.Mkdir(dead, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if err := os.WriteFile(filepath.Join(dead, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager.sendBatch = func(context.Context, string, *pb.UploadServerDailyStatsRequest) error {
		t.Fatal("over-budget accounting reached RPC")
		return nil
	}
	if err := manager.sendPending(); err == nil {
		t.Fatal("full quarantine was hidden")
	}
	if data, err := os.ReadFile(filepath.Join(dir, id+".batch")); err != nil || !bytes.Equal(data, poison) {
		t.Fatal("full quarantine discarded over-budget accounting", err)
	}
}
