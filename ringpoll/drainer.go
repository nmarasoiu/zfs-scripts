package ringpoll

import "time"

// Poller multiplexes N Readers, draining events round-robin into a
// caller-owned []Record buffer. It owns flow-control queries (Quiet,
// Closed) so consumers never think about ring fill levels.
type Poller struct {
	readers []*Reader
}

// NewPoller creates a Poller over the given Readers.
func NewPoller(readers ...*Reader) *Poller {
	return &Poller{readers: readers}
}

// FillBatch drains up to len(buf) records round-robin across all rings.
// Returns the number of records filled. Does NOT commit or sleep.
func (p *Poller) FillBatch(buf []Record) int {
	n := 0
	for _, rd := range p.readers {
		for n < len(buf) && rd.Poll(&buf[n]) {
			n++
		}
		if n == len(buf) {
			break
		}
	}
	return n
}

// CommitAll publishes consumer positions and snapshots all rings.
func (p *Poller) CommitAll() {
	for _, rd := range p.readers {
		rd.CommitAndSnap()
	}
}

// Quiet reports whether all rings are below threshold fraction of capacity.
// A threshold of 0.05 means "all rings < 5% full".
func (p *Poller) Quiet(threshold float64) bool {
	for _, rd := range p.readers {
		fill := float64(rd.Pending()) / float64(rd.BufSize())
		if fill >= threshold {
			return false
		}
	}
	return true
}

// Closed returns true when all underlying readers are closed.
func (p *Poller) Closed() bool {
	for _, rd := range p.readers {
		if !rd.Closed() {
			return false
		}
	}
	return true
}

// DrainOpts configures the Drainer loop.
type DrainOpts struct {
	MaxBatch       int           // buffer size (default 1024)
	PollSleep      time.Duration // sleep when quiet (default 3ms)
	QuietThreshold float64       // ring fill fraction to trigger sleep (default 0.05)
}

func (o DrainOpts) maxBatch() int {
	if o.MaxBatch > 0 {
		return o.MaxBatch
	}
	return 1024
}

func (o DrainOpts) pollSleep() time.Duration {
	if o.PollSleep > 0 {
		return o.PollSleep
	}
	return 3 * time.Millisecond
}

func (o DrainOpts) quietThreshold() float64 {
	if o.QuietThreshold > 0 {
		return o.QuietThreshold
	}
	return 0.05
}

// Drainer owns a Poller and a pre-allocated record buffer. It runs
// the ingestion loop: fill batch → invoke callback → flow-control sleep.
// The callback receives a slice of records valid only for the duration
// of the call. The Drainer blocks in Run until all readers are closed.
type Drainer struct {
	poller *Poller
	buf    []Record
	opts   DrainOpts
}

// NewDrainer creates a Drainer wrapping the given Poller.
func NewDrainer(poller *Poller, opts DrainOpts) *Drainer {
	sz := opts.maxBatch()
	buf := make([]Record, sz)
	return &Drainer{poller: poller, buf: buf, opts: opts}
}

// Run polls in a loop, invoking fn with each non-empty batch.
// Blocks until all underlying readers are closed.
//
// The callback receives buf[:n] — a slice into the Drainer's internal
// buffer. The slice is only valid for the duration of the call; the
// Drainer reuses the buffer on the next FillBatch.
func (d *Drainer) Run(fn func(batch []Record)) {
	sleep := d.opts.pollSleep()
	threshold := d.opts.quietThreshold()

	for !d.poller.Closed() {
		n := d.poller.FillBatch(d.buf)
		if n > 0 {
			fn(d.buf[:n])
		}
		d.poller.CommitAll()
		if d.poller.Quiet(threshold) {
			time.Sleep(sleep)
		}
	}
	// Final drain after close
	n := d.poller.FillBatch(d.buf)
	if n > 0 {
		fn(d.buf[:n])
		d.poller.CommitAll()
	}
}
