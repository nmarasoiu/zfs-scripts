// Package ringpoll provides a direct-mmap polling reader for BPF ring buffers.
//
// It replaces the epoll-based reader in cilium/ebpf with direct mmap access
// to the ring buffer's producer/consumer pages, eliminating epoll_wait syscall
// overhead entirely. Between drain cycles an adaptive [Pacer] sleeps just long
// enough for the worst-case ring to reach a target fill level, so CPU usage
// stays near zero while maintaining low latency.
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
//	rings, _ := ringpoll.NewGroup(ringMaps)
//	defer rings.Cleanup()
//	pacer := ringpoll.NewPacer(0.5, 50*time.Microsecond, 50*time.Millisecond)
//	var rec ringpoll.Record
//	for !rings.Closed() {
//	    pending, cap := rings.FillBytes()
//	    for rings.Poll(&rec) { /* process rec.RawSample */ }
//	    rings.Commit()
//	    pacer.Pace(pending, cap)
//	}
package ringpoll

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
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
// replaces separate atomic stores/loads, ensuring consistent reads.
type PollSnapshot struct {
	EventSum      int64
	NonEmptyCount int64
	PollCount     int64
	MaxPending    int64 // high-water mark of ring fill (bytes)
}

// Reader reads from a BPF ring buffer by polling the mmap'd
// producer/consumer positions directly, avoiding epoll entirely.
type Reader struct {
	consMmap  []byte  // mmap'd consumer page
	prodMmap  []byte  // mmap'd producer + data pages
	consPos   *uint64 // pointer into consMmap
	prodPos   *uint64 // pointer into prodMmap
	ring      []byte  // data region (double-mapped, so no wrap logic needed)
	mask      uint64
	pos       uint64 // current consumer position (local copy)
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

// NewReader creates a polling ring buffer reader for the given BPF map.
// The map must be of type RingBuf. The caller controls sleep timing via Poll.
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
		consMmap: cons,
		prodMmap: prod,
		consPos:  consPos,
		prodPos:  prodPos,
		ring:     prod[pageSize:],
		mask:     uint64(size - 1),
		pos:      atomic.LoadUint64(consPos),
		bufSize:  size,
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

// MaxPending returns the high-water mark of ring buffer fill in bytes.
func (r *Reader) MaxPending() int64 {
	snap := r.snapshot.Load()
	if snap == nil {
		return 0
	}
	return snap.MaxPending
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
	if r.batchCount > 0 {
		r.nonEmptyCount++
		r.eventSum += r.batchCount
		r.batchCount = 0
	}
	r.snapshot.Store(&PollSnapshot{
		EventSum:      r.eventSum,
		NonEmptyCount: r.nonEmptyCount,
		PollCount:     r.pollCount,
		MaxPending:    r.maxPending,
	})
}

// allClosed reports whether every reader in the slice has been closed.
func allClosed(readers []*Reader) bool {
	for _, rd := range readers {
		if !rd.Closed() {
			return false
		}
	}
	return true
}

// commitAll calls CommitAndSnap on every reader in the slice.
func commitAll(readers []*Reader) {
	for _, rd := range readers {
		rd.CommitAndSnap()
	}
}

// GroupSnapshot holds aggregated stats across multiple ring buffer readers.
type GroupSnapshot struct {
	Pending    int   // worst-case across rings
	MaxPending int64 // worst-case high-water mark
	Cap        int64 // per-ring capacity (all same size)
	EventSum   int64 // sum across rings
	NonEmpty   int64 // sum across rings
	PollCount  int64 // sum across rings
}

// snapshotGroup aggregates stats across multiple ring buffer readers.
// Capacity metrics use worst-case; counters are summed.
func snapshotGroup(readers []*Reader) GroupSnapshot {
	var g GroupSnapshot
	for _, rd := range readers {
		if pending := rd.Pending(); pending > g.Pending {
			g.Pending = pending
		}
		g.Cap = int64(rd.BufSize())
		snap := rd.Snapshot()
		if snap == nil {
			continue
		}
		if snap.MaxPending > g.MaxPending {
			g.MaxPending = snap.MaxPending
		}
		g.EventSum += snap.EventSum
		g.NonEmpty += snap.NonEmptyCount
		g.PollCount += snap.PollCount
	}
	return g
}

// Group manages multiple ring buffer readers, providing a single polling
// interface that scans all rings in order.
type Group struct {
	readers []*Reader
}

// NewGroup creates a Group of polling readers, one per map.
// On partial failure, already-opened readers are cleaned up.
func NewGroup(maps []*ebpf.Map) (*Group, error) {
	readers := make([]*Reader, len(maps))
	for i, m := range maps {
		rd, err := NewReader(m)
		if err != nil {
			for j := 0; j < i; j++ {
				readers[j].Cleanup()
			}
			return nil, fmt.Errorf("open ring buffer %d: %w", i, err)
		}
		readers[i] = rd
	}
	return &Group{readers: readers}, nil
}

// Poll scans rings 0..N-1 and returns the first record found.
// Returns false when all rings are empty in a single pass.
func (g *Group) Poll(rec *Record) bool {
	for _, rd := range g.readers {
		if rd.Poll(rec) {
			return true
		}
	}
	return false
}

// Commit publishes consumer positions and snapshots stats for all readers.
func (g *Group) Commit() {
	commitAll(g.readers)
}

// MaxFill returns the maximum fill fraction (0.0–1.0) across all rings.
// Call before draining to capture pre-drain lag; use the result after
// draining to decide whether to sleep.
func (g *Group) MaxFill() float64 {
	var max float64
	for _, rd := range g.readers {
		fill := float64(rd.Pending()) / float64(rd.BufSize())
		if fill > max {
			max = fill
		}
	}
	return max
}

// FillBytes returns the max pending bytes across all rings and the per-ring capacity.
func (g *Group) FillBytes() (maxPending, capacity int) {
	for _, rd := range g.readers {
		if p := rd.Pending(); p > maxPending {
			maxPending = p
		}
		capacity = rd.BufSize()
	}
	return
}

// FillDetail returns the max and average pending bytes across all rings, plus capacity.
func (g *Group) FillDetail() (maxPending, avgPending, capacity int) {
	sum := 0
	for _, rd := range g.readers {
		p := rd.Pending()
		sum += p
		if p > maxPending {
			maxPending = p
		}
		capacity = rd.BufSize()
	}
	if len(g.readers) > 0 {
		avgPending = sum / len(g.readers)
	}
	return
}

// Closed reports whether all readers have been closed.
func (g *Group) Closed() bool {
	return allClosed(g.readers)
}

// Snapshot returns aggregated stats across all ring buffer readers.
func (g *Group) Snapshot() GroupSnapshot {
	return snapshotGroup(g.readers)
}

// Close signals all readers to stop.
func (g *Group) Close() {
	for _, rd := range g.readers {
		rd.Close()
	}
}

// Cleanup unmaps shared memory for all readers.
func (g *Group) Cleanup() {
	for _, rd := range g.readers {
		rd.Cleanup()
	}
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

