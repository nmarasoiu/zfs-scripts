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
// Usage:
//
//	rd, err := ringpoll.NewReader(objs.Events, 50*time.Microsecond)
//	if err != nil { log.Fatal(err) }
//	defer rd.Close()
//	defer rd.Cleanup()
//
//	var rec ringpoll.Record
//	for rd.ReadInto(&rec) {
//	    // process rec.RawSample
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

// Reader reads from a BPF ring buffer by busy-polling the mmap'd
// producer/consumer positions directly, avoiding epoll entirely.
type Reader struct {
	consMmap  []byte  // mmap'd consumer page
	prodMmap  []byte  // mmap'd producer + data pages
	consPos   *uint64 // pointer into consMmap
	prodPos   *uint64 // pointer into prodMmap
	ring      []byte  // data region (double-mapped, so no wrap logic needed)
	mask      uint64
	pos       uint64 // current consumer position (local copy)
	closed    atomic.Bool
	pollSleep time.Duration

	// Stats (written by reader goroutine, read atomically by display)
	batchCount int64        // events in current batch (reader goroutine only)
	lastBatch  atomic.Int64 // events drained in last completed batch
	bufSize    int          // ring capacity in bytes

	// Poll stats (written by reader goroutine at ring-empty boundaries)
	eventSum      int64 // running sum of events across non-empty batches
	nonEmptyCount int64 // number of non-empty batches observed
	pollCount     int64 // total ring-empty observations (empty + non-empty endings)

	// Atomic copies for display goroutine
	atomicEventSum      atomic.Int64
	atomicNonEmptyCount atomic.Int64
	atomicPollCount     atomic.Int64
	lastNonEmpty        atomic.Int64 // batch size of most recent non-empty poll (last1)
	lastEmptyNano       atomic.Int64 // UnixNano of most recent empty poll (last0)
	maxPending          atomic.Int64 // high-water mark of ring fill (bytes)
}

// NewReader creates a busy-polling ring buffer reader for the given BPF map.
// The map must be of type RingBuf. pollSleep controls how long the reader
// sleeps when the ring is empty (50us is a good starting point).
func NewReader(m *ebpf.Map, pollSleep time.Duration) (*Reader, error) {
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
		ring:      prod[pageSize:],
		mask:      uint64(size - 1),
		pos:       atomic.LoadUint64(consPos),
		pollSleep: pollSleep,
		bufSize:   size,
	}, nil
}

// Close signals the reader to stop. The read loop will return false on the
// next iteration. Call Cleanup after the read goroutine has exited.
func (r *Reader) Close() {
	r.closed.Store(true)
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

// LastBatch returns the number of events drained in the last completed batch.
func (r *Reader) LastBatch() int64 {
	return r.lastBatch.Load()
}

// BufSize returns the ring buffer capacity in bytes.
func (r *Reader) BufSize() int {
	return r.bufSize
}

// MaxPending returns the high-water mark of ring buffer fill in bytes.
func (r *Reader) MaxPending() int64 {
	return r.maxPending.Load()
}

// PollStats returns ring poll statistics for display.
//   - avg1: average batch size of non-empty polls
//   - avg0: average batch size of all polls (including empty)
//   - last1: batch size of most recent non-empty poll
//   - last0: time since last empty poll
func (r *Reader) PollStats() (avg1, avg0 float64, last1 int64, last0 time.Duration) {
	eSum := r.atomicEventSum.Load()
	neCount := r.atomicNonEmptyCount.Load()
	pCount := r.atomicPollCount.Load()
	last1 = r.lastNonEmpty.Load()
	lastNano := r.lastEmptyNano.Load()

	if neCount > 0 {
		avg1 = float64(eSum) / float64(neCount)
	}
	if pCount > 0 {
		avg0 = float64(eSum) / float64(pCount)
	}
	if lastNano > 0 {
		last0 = time.Since(time.Unix(0, lastNano))
	}
	return
}

// Commit publishes the current consumer position to the kernel, freeing
// ring buffer space. Call after processing a batch of events.
func (r *Reader) Commit() {
	atomic.StoreUint64(r.consPos, r.pos)
}

// ReadInto polls the ring buffer for the next committed record.
// Does NOT advance the kernel-visible consumer position — call Commit()
// after processing a batch to free ring space.
// Sleeps briefly when the ring is empty (commits before sleeping so the
// kernel sees freed space). Returns true on success, false when closed.
func (r *Reader) ReadInto(rec *Record) bool {
	for {
		if r.closed.Load() {
			return false
		}

		prod := atomic.LoadUint64(r.prodPos)
		if pending := int64(prod - r.pos); pending > r.maxPending.Load() {
			r.maxPending.Store(pending)
		}
		if r.pos == prod {
			// Ring empty — commit before sleeping so kernel sees freed space
			r.Commit()
			// Record completed batch and poll stats
			r.pollCount++
			if r.batchCount > 0 {
				r.nonEmptyCount++
				r.eventSum += r.batchCount
				r.lastNonEmpty.Store(r.batchCount)
				r.lastBatch.Store(r.batchCount)
				r.batchCount = 0
				r.atomicEventSum.Store(r.eventSum)
				r.atomicNonEmptyCount.Store(r.nonEmptyCount)
			}
			r.atomicPollCount.Store(r.pollCount)
			r.lastEmptyNano.Store(time.Now().UnixNano())
			time.Sleep(r.pollSleep)
			continue
		}

		// Read 8-byte header
		off := r.pos & r.mask
		hdrLen := binary.LittleEndian.Uint32(r.ring[off:])

		if hdrLen&bpfRingbufBusyBit != 0 {
			// Producer reserved but hasn't committed yet
			time.Sleep(r.pollSleep)
			continue
		}

		r.pos += ringbufHdrSize

		dataLen := hdrLen & ^uint32(bpfRingbufBusyBit|bpfRingbufDiscardBit)
		dataAligned := (uint64(dataLen) + 7) &^ 7

		if hdrLen&bpfRingbufDiscardBit != 0 {
			r.pos += dataAligned
			continue
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
