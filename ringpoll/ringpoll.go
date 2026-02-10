// Package ringpoll provides a busy-polling reader for BPF ring buffers.
//
// It replaces the epoll-based reader in cilium/ebpf with direct mmap access
// to the ring buffer's producer/consumer pages, eliminating epoll_wait syscall
// overhead entirely. This is critical for high-throughput eBPF tracing where
// the ring buffer has near-continuous data flow.
//
// On a system tracing 180K syscalls/sec, cilium/ebpf's default epoll reader
// generated 42K epoll_wait calls/sec just to drain the ring buffer. With
// ringpoll, epoll_wait drops to zero and total reader overhead falls from
// ~110K syscalls/sec to ~5K/sec.
//
// Kernel side: pair this with bpf_ringbuf_submit(data, BPF_RB_NO_WAKEUP)
// to suppress wakeup notifications that would otherwise fire on every submit.
//
// Usage (Poll-based — caller controls sleep):
//
//	rd, err := ringpoll.NewReader(objs.Events)
//	if err != nil { log.Fatal(err) }
//	defer rd.Close()
//	defer rd.Cleanup()
//
//	var rec ringpoll.Record
//	for !rd.Closed() {
//	    for rd.Poll(&rec) {
//	        // process rec.RawSample
//	    }
//	    if rd.Pending()*20 <= rd.BufSize() { // ring < 5% full
//	        rd.CommitAndSnap()
//	        time.Sleep(pollSleep)
//	    }
//	}
package ringpoll

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
)

const (
	bpfRingbufBusyBit    = 1 << 31
	bpfRingbufDiscardBit = 1 << 30
	ringbufHdrSize       = 8 // uint32 Len + uint32 PgOff
)

// Record holds a single ring buffer sample.
type Record struct {
	RawSample []byte
}

// PollSnapshot is an atomic point-in-time snapshot of ring poll statistics,
// published once per ring-empty boundary. A single atomic.Pointer swap
// replaces 7 separate atomic stores/loads, ensuring consistent reads.
type PollSnapshot struct {
	EventSum      int64
	NonEmptyCount int64
	PollCount     int64
	LastNonEmpty  int64         // batch size of most recent non-empty poll
	LastEmptyNano int64         // UnixNano of most recent empty poll
	MaxPending    int64         // high-water mark of ring fill (bytes)
	LastBatch     int64         // events drained in last completed batch
}

// Reader reads from a BPF ring buffer by busy-polling the mmap'd
// producer/consumer positions directly, avoiding epoll entirely.
type Reader struct {
	consMmap  []byte  // mmap'd consumer page
	prodMmap  []byte  // mmap'd producer + data pages
	consPos   *uint64 // pointer into consMmap
	prodPos   *uint64 // pointer into prodMmap
	ring    []byte // data region (double-mapped, so no wrap logic needed)
	mask    uint64
	pos     uint64 // current consumer position (local copy)
	closed  atomic.Bool
	bufSize int // ring capacity in bytes

	// Local stats (written only by reader goroutine — no sync needed)
	batchCount    int64
	eventSum      int64
	nonEmptyCount int64
	pollCount     int64
	maxPending    int64

	// Single atomic snapshot published at ring-empty boundaries
	snapshot atomic.Pointer[PollSnapshot]
}

// NewReader creates a busy-polling ring buffer reader for the given BPF map.
// The map must be of type RingBuf. The reader never sleeps — the caller
// controls sleep timing via Poll() + client-side time.Sleep.
func NewReader(m *ebpf.Map) (*Reader, error) {
	if m.Type() != ebpf.RingBuf {
		return nil, fmt.Errorf("ringpoll: expected RingBuf map, got %s", m.Type())
	}

	size := int(m.MaxEntries())
	pageSize := os.Getpagesize()
	fd := m.FD()

	// Consumer page: read-write (we update consumer position)
	cons, err := syscall.Mmap(fd, 0, pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("ringpoll: mmap consumer: %w", err)
	}

	// Producer page + data (double-mapped ring): read-only
	prod, err := syscall.Mmap(fd, int64(pageSize), pageSize+2*size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		syscall.Munmap(cons)
		return nil, fmt.Errorf("ringpoll: mmap producer: %w", err)
	}

	consPos := (*uint64)(unsafe.Pointer(&cons[0]))
	prodPos := (*uint64)(unsafe.Pointer(&prod[0]))

	return &Reader{
		consMmap:  cons,
		prodMmap:  prod,
		consPos:   consPos,
		prodPos:   prodPos,
		ring:    prod[pageSize:],
		mask:    uint64(size - 1),
		pos:     atomic.LoadUint64(consPos),
		bufSize: size,
	}, nil
}

// Close signals the reader to stop. The read loop will return false on the
// next iteration. Call Cleanup after the read goroutine has exited.
func (r *Reader) Close() {
	r.closed.Store(true)
}

// Closed returns true if the reader has been closed.
func (r *Reader) Closed() bool {
	return r.closed.Load()
}

// Cleanup unmaps the shared memory. Must be called after the read loop exits.
func (r *Reader) Cleanup() {
	syscall.Munmap(r.prodMmap)
	syscall.Munmap(r.consMmap)
}

// Pending returns the current ring buffer fill level in bytes.
func (r *Reader) Pending() int {
	prod := atomic.LoadUint64(r.prodPos)
	cons := atomic.LoadUint64(r.consPos)
	return int(prod - cons)
}

// BufSize returns the ring buffer capacity in bytes.
func (r *Reader) BufSize() int {
	return r.bufSize
}

// Snapshot returns the latest poll statistics snapshot, or nil if none published yet.
func (r *Reader) Snapshot() *PollSnapshot {
	return r.snapshot.Load()
}

// PollStats returns ring poll statistics for display.
//   - avg1: average batch size of non-empty polls
//   - avg0: average batch size of all polls (including empty)
//   - last1: batch size of most recent non-empty poll
//   - last0: time since last empty poll
func (r *Reader) PollStats() (avg1, avg0 float64, last1 int64, last0 time.Duration) {
	snap := r.snapshot.Load()
	if snap == nil {
		return
	}
	if snap.NonEmptyCount > 0 {
		avg1 = float64(snap.EventSum) / float64(snap.NonEmptyCount)
	}
	if snap.PollCount > 0 {
		avg0 = float64(snap.EventSum) / float64(snap.PollCount)
	}
	last1 = snap.LastNonEmpty
	if snap.LastEmptyNano > 0 {
		last0 = time.Since(time.Unix(0, snap.LastEmptyNano))
	}
	return
}

// MaxPending returns the high-water mark of ring buffer fill in bytes.
func (r *Reader) MaxPending() int64 {
	snap := r.snapshot.Load()
	if snap == nil {
		return 0
	}
	return snap.MaxPending
}

// LastBatch returns the number of events drained in the last completed batch.
func (r *Reader) LastBatch() int64 {
	snap := r.snapshot.Load()
	if snap == nil {
		return 0
	}
	return snap.LastBatch
}

// Commit publishes the current consumer position to the kernel, freeing
// ring buffer space. Call after processing a batch of events.
func (r *Reader) Commit() {
	atomic.StoreUint64(r.consPos, r.pos)
}

// CommitAndSnap publishes the consumer position AND publishes the poll
// snapshot in one call. Used by the multi-ring drainer at ring-empty
// boundaries to atomically free space and update stats.
func (r *Reader) CommitAndSnap() {
	r.Commit()
	r.pollCount++
	lastBatch := r.batchCount
	if r.batchCount > 0 {
		r.nonEmptyCount++
		r.eventSum += r.batchCount
		r.batchCount = 0
	}
	r.snapshot.Store(&PollSnapshot{
		EventSum:      r.eventSum,
		NonEmptyCount: r.nonEmptyCount,
		PollCount:     r.pollCount,
		LastNonEmpty:  lastBatch,
		LastEmptyNano: time.Now().UnixNano(),
		MaxPending:    r.maxPending,
		LastBatch:     lastBatch,
	})
}

// Poll is a non-blocking read from the ring buffer. Returns true if a
// record was read, false immediately when the ring is empty or the
// producer is mid-write. The caller controls sleep timing across
// multiple rings.
func (r *Reader) Poll(rec *Record) bool {
	if r.closed.Load() {
		return false
	}

	for {
		prod := atomic.LoadUint64(r.prodPos)
		if pending := int64(prod - r.pos); pending > r.maxPending {
			r.maxPending = pending
		}
		if r.pos == prod {
			return false // empty
		}

		// Read 8-byte header
		off := r.pos & r.mask
		hdrLen := binary.LittleEndian.Uint32(r.ring[off:])

		if hdrLen&bpfRingbufBusyBit != 0 {
			return false // producer mid-write
		}

		r.pos += ringbufHdrSize

		dataLen := hdrLen & ^uint32(bpfRingbufBusyBit|bpfRingbufDiscardBit)
		dataAligned := (uint64(dataLen) + 7) &^ 7

		if hdrLen&bpfRingbufDiscardBit != 0 {
			r.pos += dataAligned
			continue // skip discarded record
		}

		// Copy data out of the ring
		start := r.pos & r.mask
		if cap(rec.RawSample) < int(dataLen) {
			rec.RawSample = make([]byte, dataLen)
		} else {
			rec.RawSample = rec.RawSample[:dataLen]
		}
		copy(rec.RawSample, r.ring[start:start+uint64(dataLen)])

		r.pos += dataAligned

		r.batchCount++
		return true
	}
}

