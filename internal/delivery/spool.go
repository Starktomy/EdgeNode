// Package delivery implements bounded, durable batches for at-least-once delivery.
package delivery

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	var total int64
	var count int
	if err := s.scanFiles(func(_ string, info fs.FileInfo) error {
		total += info.Size()
		count++
		return nil
	}); err != nil {
		return "", err
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

// Pending returns the oldest batches within one maxBatch payload budget.
// Senders should use WalkPending to avoid retaining several payloads at once.
func (s *Spool) Pending() ([]Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates, count, err := s.pendingCandidatesLocked()
	if err != nil || count == 0 {
		return nil, err
	}
	result := make([]Batch, 0, count)
	remaining := s.maxBatch
	for _, entry := range candidates[:count] {
		batch, err := s.readBatchLocked(entry.name, remaining)
		if errors.Is(err, errPendingBudget) {
			break
		}
		if err != nil {
			return nil, err
		}
		if batch.Data != nil {
			result = append(result, batch)
			remaining -= int64(len(batch.Data))
		}
	}
	return result, nil
}

// WalkPending visits at most eight oldest batches, reading one bounded payload
// immediately before its visit. No spool lock is held during visit; it may Ack,
// Quarantine, or wait for a network response without blocking Append. A visit
// error stops the walk and leaves unacknowledged identities on disk for retry.
// Callbacks should not retain Data beyond the visit.
func (s *Spool) WalkPending(visit func(Batch) error) error {
	s.mu.Lock()
	candidates, count, err := s.pendingCandidatesLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, entry := range candidates[:count] {
		s.mu.Lock()
		batch, err := s.readBatchLocked(entry.name, s.maxBatch)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if batch.Data == nil {
			continue
		}
		err = visit(batch)
		batch.Data = nil
		if err != nil {
			return err
		}
	}
	return nil
}

type spoolCandidate struct {
	name     string
	modified int64
}

func (s *Spool) pendingCandidatesLocked() ([8]spoolCandidate, int, error) {
	// Retain only the oldest window while scanning. A full backlog no longer
	// allocates/sorts thousands of metadata records just to send eight batches.
	var oldest [8]spoolCandidate
	var count int
	err := s.scanFiles(func(name string, info fs.FileInfo) error {
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > s.maxBatch {
			return s.quarantineLocked(strings.TrimSuffix(name, ".batch"), "invalid spool file type or size")
		}
		item := spoolCandidate{name, info.ModTime().UnixNano()}
		less := func(a, b spoolCandidate) bool {
			return a.modified < b.modified || a.modified == b.modified && a.name < b.name
		}
		position := count
		if count == len(oldest) {
			if !less(item, oldest[count-1]) {
				return nil
			}
			position--
		} else {
			count++
		}
		for position > 0 && less(item, oldest[position-1]) {
			oldest[position] = oldest[position-1]
			position--
		}
		oldest[position] = item
		return nil
	})
	if os.IsNotExist(err) {
		return oldest, 0, nil
	}
	return oldest, count, err
}

var errPendingBudget = errors.New("delivery pending payload budget reached")

// Revalidate immediately before reading: restored/replaced files may have
// changed after selection. Never follow symlinks or read an unbounded payload.
func (s *Spool) readBatchLocked(name string, budget int64) (Batch, error) {
	id := strings.TrimSuffix(name, ".batch")
	path := filepath.Join(s.dir, name)
	isolate := func() (Batch, error) {
		return Batch{}, s.quarantineLocked(id, "invalid or changed spool file type or size")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Batch{}, nil
	}
	if err != nil {
		return Batch{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > s.maxBatch {
		return isolate()
	}
	file, err := openBatch(path)
	if os.IsNotExist(err) {
		return Batch{}, nil
	}
	if err != nil {
		// A replacement between Lstat and open must not wedge valid batches.
		if current, statErr := os.Lstat(path); statErr == nil && !current.Mode().IsRegular() {
			return isolate()
		}
		return Batch{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Batch{}, err
	}
	if !opened.Mode().IsRegular() || opened.Size() <= 0 || opened.Size() > s.maxBatch || !os.SameFile(info, opened) {
		_ = file.Close()
		return isolate()
	}
	if opened.Size() > budget {
		_ = file.Close()
		return Batch{}, errPendingBudget
	}
	// One extra byte detects growth without ReadAll's repeated buffer growth.
	data := make([]byte, int(opened.Size())+1)
	n, readErr := io.ReadFull(file, data)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return Batch{}, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return Batch{}, closeErr
	}
	if n != len(data)-1 || !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return isolate()
	}
	return Batch{ID: id, Data: data[:n]}, nil
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

// scanFiles uses bounded directory pages and avoids os.ReadDir's global name
// sort. Read current metadata on every operation so externally restored files
// and removals still participate in capacity accounting and delivery.
func (s *Spool) scanFiles(visit func(string, fs.FileInfo) error) error {
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		names, readErr := directory.Readdirnames(128)
		for _, name := range names {
			if !validName(name) {
				continue
			}
			info, err := os.Lstat(filepath.Join(s.dir, name))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := visit(name, info); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func validName(name string) bool {
	if !strings.HasSuffix(name, ".batch") || len(name) != 38 {
		return false
	}
	for i := 0; i < 32; i++ {
		c := name[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
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
