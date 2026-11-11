// Package delivery implements bounded, durable batches for at-least-once delivery.
package delivery

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TeaOSLab/EdgeNode/internal/utils/atomicfile"
)

var ErrFull = errors.New("delivery spool is full")

type Batch struct {
	ID   string
	Data []byte
}

type Spool struct {
	dir         string
	maxBytes    int64
	maxBatch    int64
	mu          sync.Mutex
	write       func(string, []byte) (bool, error)
	syncDir     func(string) error
	quarantined atomic.Uint64
}

func NewSpool(dir string, maxBytes, maxBatch int64) *Spool {
	return &Spool{dir: dir, maxBytes: maxBytes, maxBatch: maxBatch, write: atomicfile.WriteCommitted, syncDir: atomicfile.SyncDir}
}

// Append returns a nonempty ID once a batch has been published. An error with
// a nonempty ID means durability confirmation failed; callers MUST NOT restore
// or append the same data under a different identity. Pending can retry this ID.
func (s *Spool) Append(data []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) == 0 || int64(len(data)) > s.maxBatch {
		return "", errors.New("invalid delivery batch size")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", err
	}
	var total int64
	var count int
	for _, entry := range entries {
		if !validName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		total += info.Size()
		count++
	}
	if total+int64(len(data)) > s.maxBytes || count >= 4096 {
		return "", ErrFull
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	published, err := s.write(filepath.Join(s.dir, id+".batch"), data)
	if published {
		return id, err
	}
	return "", err
}

func (s *Spool) Pending() ([]Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Oldest first prevents newer traffic from starving an earlier failed batch.
	type candidate struct {
		name     string
		modified int64
	}
	var candidates []candidate
	for _, entry := range entries {
		if !validName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > s.maxBatch {
			if err := s.quarantineLocked(strings.TrimSuffix(entry.Name(), ".batch"), "invalid spool file type or size"); err != nil {
				return nil, err
			}
			continue
		}
		candidates = append(candidates, candidate{entry.Name(), info.ModTime().UnixNano()})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modified < candidates[j].modified })
	// Return only a bounded window so a full spool does not become a RAM queue.
	if len(candidates) > 8 {
		candidates = candidates[:8]
	}
	result := make([]Batch, 0, len(candidates))
	for _, entry := range candidates {
		data, err := os.ReadFile(filepath.Join(s.dir, entry.name))
		if err != nil {
			return nil, err
		}
		result = append(result, Batch{ID: strings.TrimSuffix(entry.name, ".batch"), Data: data})
	}
	return result, nil
}

func (s *Spool) Ack(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validName(id + ".batch") {
		return errors.New("invalid delivery batch ID")
	}
	err := os.Remove(filepath.Join(s.dir, id+".batch"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.syncDir(s.dir)
}

func validName(name string) bool {
	if !strings.HasSuffix(name, ".batch") || len(name) != 38 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(name, ".batch"))
	return err == nil
}

// Quarantined counts batches isolated in this process. They remain on disk for
// diagnosis/replay and are never reported as delivered or silently discarded.
func (s *Spool) Quarantined() uint64 { return s.quarantined.Load() }

// Quarantine moves a permanently invalid batch out of the sending head. The
// diagnostic budget is separate and bounded; when full, failure stays visible
// and the original batch is retained rather than deleting accounting data.
func (s *Spool) Quarantine(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quarantineLocked(id, reason)
}

func (s *Spool) quarantineLocked(id, reason string) error {
	if !validName(id + ".batch") {
		return errors.New("invalid quarantine batch identity")
	}
	source := filepath.Join(s.dir, id+".batch")
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	deadDir := filepath.Join(s.dir, "quarantine")
	if err := os.MkdirAll(deadDir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(deadDir)
	if err != nil {
		return err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
	}
	budget := s.maxBytes
	if budget > 64<<20 {
		budget = 64 << 20
	}
	if budget < s.maxBatch+4096 {
		budget = s.maxBatch + 4096
	}
	if len(entries) >= 128 || total+info.Size()+4096 > budget {
		return errors.New("delivery quarantine budget exhausted; operator action required")
	}
	target := filepath.Join(deadDir, id+".batch")
	if err := os.Rename(source, target); err != nil {
		return err
	}
	s.quarantined.Add(1)
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	detail, _ := json.Marshal(struct {
		BatchID string `json:"batchId"`
		Reason  string `json:"reason"`
		Time    string `json:"time"`
	}{id, reason, time.Now().UTC().Format(time.RFC3339)})
	_, writeErr := s.write(filepath.Join(deadDir, id+".reason.json"), detail)
	return errors.Join(writeErr, s.syncDir(deadDir), s.syncDir(s.dir))
}
