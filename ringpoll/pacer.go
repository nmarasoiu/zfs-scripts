package ringpoll

import "time"

// Pacer implements adaptive sleep for ring buffer polling.
// It targets waking up when the worst-case ring reaches a configurable
// fill fraction, using an EMA of the observed produce rate.
type Pacer struct {
	targetFill float64
	minSleep   time.Duration
	maxSleep   time.Duration
	alpha      float64
	emaRate    float64   // bytes/sec EMA
	lastWake   time.Time

	// Running average of sleep durations (for observability).
	sleepSum   time.Duration
	sleepCount int64

	// Pure (non-zero) sleep subset.
	pureSleepSum   time.Duration
	pureSleepCount int64
}

// NewPacer creates a Pacer with the given target fill fraction and sleep bounds.
// targetFill is the desired fill level (0.0–1.0) when waking, typically 0.5.
func NewPacer(targetFill float64, minSleep, maxSleep time.Duration) *Pacer {
	return &Pacer{
		targetFill: targetFill,
		minSleep:   minSleep,
		maxSleep:   maxSleep,
		alpha:      0.3,
	}
}

// Pace updates the rate estimate from observed fill and sleeps adaptively.
// pendingBytes is the max pending across rings BEFORE draining; capacity is
// the per-ring buffer size. Returns the duration slept (0 if fill >= target).
func (p *Pacer) Pace(pendingBytes, capacity int) time.Duration {
	now := time.Now()

	track := func(d time.Duration) {
		p.sleepSum += d
		p.sleepCount++
		if d > 0 {
			p.pureSleepSum += d
			p.pureSleepCount++
		}
	}

	// First cycle: initialize lastWake, sleep minSleep.
	if p.lastWake.IsZero() {
		p.lastWake = now
		time.Sleep(p.minSleep)
		track(p.minSleep)
		return p.minSleep
	}

	cycleDur := now.Sub(p.lastWake)

	// No events observed — don't decay EMA, just sleep minSleep to stay responsive.
	if pendingBytes <= 0 {
		p.lastWake = now
		time.Sleep(p.minSleep)
		track(p.minSleep)
		return p.minSleep
	}

	// Update produce rate EMA: bytes/sec observed this cycle.
	if cycleDur > 0 {
		rate := float64(pendingBytes) / cycleDur.Seconds()
		if p.emaRate <= 0 {
			p.emaRate = rate // seed
		} else {
			p.emaRate = p.alpha*rate + (1-p.alpha)*p.emaRate
		}
	}

	// If fill >= target, we're behind — don't sleep, just update rate.
	fill := float64(pendingBytes) / float64(capacity)
	if fill >= p.targetFill {
		p.lastWake = now
		track(0)
		return 0
	}

	// Compute adaptive sleep: time for ring to fill from 0 to target.
	sleep := time.Duration(p.targetFill * float64(capacity) / p.emaRate * float64(time.Second))

	// Clamp to bounds.
	if sleep < p.minSleep {
		sleep = p.minSleep
	} else if sleep > p.maxSleep {
		sleep = p.maxSleep
	}

	time.Sleep(sleep)
	p.lastWake = time.Now()
	track(sleep)
	return sleep
}

// SleepStats holds both pure (non-zero) and all-inclusive sleep averages.
type SleepStats struct {
	Pure time.Duration // average of only non-zero sleeps
	All  time.Duration // average including zero-sleep (busy) cycles
}

// AvgSleep returns the running average sleep duration (all cycles, including zero).
func (p *Pacer) AvgSleep() time.Duration {
	if p.sleepCount == 0 {
		return 0
	}
	return p.sleepSum / time.Duration(p.sleepCount)
}

// SleepAvgs returns both the pure (non-zero only) and all-inclusive sleep averages.
func (p *Pacer) SleepAvgs() SleepStats {
	var s SleepStats
	if p.pureSleepCount > 0 {
		s.Pure = p.pureSleepSum / time.Duration(p.pureSleepCount)
	}
	if p.sleepCount > 0 {
		s.All = p.sleepSum / time.Duration(p.sleepCount)
	}
	return s
}
