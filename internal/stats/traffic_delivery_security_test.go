package stats

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/delivery"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	manager.itemMap[strconv.FormatInt(time.Now().Unix()/300*300, 10)+"1"] = &TrafficItem{Bytes: 7, CountRequests: 1}
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
		manager.itemMap[strconv.FormatInt(now-int64(i)*300, 10)+"1"] = &TrafficItem{Bytes: 1, CountRequests: 1}
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
	manager.itemMap[strconv.FormatInt(time.Now().Unix()/300*300, 10)+"1"] = &TrafficItem{Bytes: 19, CountRequests: 1}
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
