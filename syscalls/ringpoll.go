package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
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

// PollRecord holds a single ring buffer sample.
type PollRecord struct {
	RawSample []byte
}

// RingPollReader reads from a BPF ring buffer by busy-polling the mmap'd
// producer/consumer positions directly, avoiding epoll entirely.
type RingPollReader struct {
	consMmap []byte  // mmap'd consumer page
	prodMmap []byte  // mmap'd producer + data pages
	consPos  *uint64 // pointer into consMmap
	prodPos  *uint64 // pointer into prodMmap
	ring     []byte  // data region (double-mapped, so no wrap logic needed)
	mask     uint64
	pos      uint64 // current consumer position (local copy)
	closed   atomic.Bool
}

// NewRingPollReader creates a busy-polling ring buffer reader for the given map.
func NewRingPollReader(m *ebpf.Map) (*RingPollReader, error) {
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

	return &RingPollReader{
		consMmap: cons,
		prodMmap: prod,
		consPos:  consPos,
		prodPos:  prodPos,
		ring:     prod[pageSize:],
		mask:     uint64(size - 1),
		pos:      atomic.LoadUint64(consPos),
	}, nil
}

// Close signals the reader to stop. The read loop will return false on the
// next iteration. Call Cleanup after the read goroutine has exited.
func (r *RingPollReader) Close() {
	r.closed.Store(true)
}

// Cleanup unmaps the shared memory. Must be called after the read loop exits.
func (r *RingPollReader) Cleanup() {
	syscall.Munmap(r.prodMmap)
	syscall.Munmap(r.consMmap)
}

// ReadInto busy-polls the ring buffer for the next committed record.
// Returns true on success, false when the reader has been closed.
func (r *RingPollReader) ReadInto(rec *PollRecord) bool {
	for {
		if r.closed.Load() {
			return false
		}

		prod := atomic.LoadUint64(r.prodPos)
		if r.pos == prod {
			runtime.Gosched()
			continue
		}

		// Read 8-byte header
		off := r.pos & r.mask
		hdrLen := binary.LittleEndian.Uint32(r.ring[off:])

		if hdrLen&bpfRingbufBusyBit != 0 {
			// Producer reserved but hasn't committed yet
			runtime.Gosched()
			continue
		}

		r.pos += ringbufHdrSize

		dataLen := hdrLen & ^uint32(bpfRingbufBusyBit|bpfRingbufDiscardBit)
		dataAligned := (uint64(dataLen) + 7) &^ 7

		if hdrLen&bpfRingbufDiscardBit != 0 {
			r.pos += dataAligned
			atomic.StoreUint64(r.consPos, r.pos)
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
		atomic.StoreUint64(r.consPos, r.pos)

		return true
	}
}
