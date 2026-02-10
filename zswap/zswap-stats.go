package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	zswapDebug = "/sys/kernel/debug/zswap"
	zswapParam = "/sys/module/zswap/parameters"
)

type stats struct {
	storedPages      int64
	poolSize         int64
	writtenBack      int64
	rejectPoor       int64
	rejectReclaim    int64
	sameFilled       int64
	maxPoolPct       int64
	totalRAM         int64
	writebackEnabled bool // legacy kernels
	shrinkerEnabled  bool // modern kernels (5.18+)
}

type minMax struct {
	min, max int64
	set      bool
}

func (m *minMax) update(v int64) {
	if !m.set {
		m.min, m.max = v, v
		m.set = true
	} else {
		if v < m.min {
			m.min = v
		}
		if v > m.max {
			m.max = v
		}
	}
}

// ── long-lived FDs ──────────────────────────────────────────────

type zswapFds struct {
	storedPages   int
	poolSize      int
	writtenBack   int
	rejectPoor    int
	rejectReclaim int
	sameFilled    int
	maxPoolPct    int
	meminfo       int
	writeback     int
	shrinker      int
}

func tryOpen(path string) int {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return -1
	}
	return fd
}

func openZswapFds() zswapFds {
	return zswapFds{
		storedPages:   tryOpen(zswapDebug + "/stored_pages"),
		poolSize:      tryOpen(zswapDebug + "/pool_total_size"),
		writtenBack:   tryOpen(zswapDebug + "/written_back_pages"),
		rejectPoor:    tryOpen(zswapDebug + "/reject_compress_poor"),
		rejectReclaim: tryOpen(zswapDebug + "/reject_reclaim_fail"),
		sameFilled:    tryOpen(zswapDebug + "/same_filled_pages"),
		maxPoolPct:    tryOpen(zswapParam + "/max_pool_percent"),
		meminfo:       tryOpen("/proc/meminfo"),
		writeback:     tryOpen(zswapParam + "/writeback"),
		shrinker:      tryOpen(zswapParam + "/shrinker_enabled"),
	}
}

func (fds *zswapFds) close() {
	for _, fd := range []int{fds.storedPages, fds.poolSize, fds.writtenBack,
		fds.rejectPoor, fds.rejectReclaim, fds.sameFilled, fds.maxPoolPct,
		fds.meminfo, fds.writeback, fds.shrinker} {
		if fd >= 0 {
			syscall.Close(fd)
		}
	}
}

func pread(fd int, buf []byte) int {
	n, _ := syscall.Pread(fd, buf, 0)
	return n
}

// ── reading ─────────────────────────────────────────────────────

func readInt64(fd int, buf []byte) int64 {
	n := pread(fd, buf)
	if n <= 0 {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(buf[:n])), 10, 64)
	return v
}

func readBool(fd int, buf []byte) bool {
	n := pread(fd, buf)
	if n <= 0 {
		return false
	}
	s := strings.TrimSpace(string(buf[:n]))
	return s == "Y" || s == "1" || s == "y" || s == "yes"
}

func getTotalRAM(fd int, buf []byte) int64 {
	n := pread(fd, buf)
	if n <= 0 {
		return 0
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func readStats(fds *zswapFds, buf []byte) stats {
	return stats{
		storedPages:      readInt64(fds.storedPages, buf),
		poolSize:         readInt64(fds.poolSize, buf),
		writtenBack:      readInt64(fds.writtenBack, buf),
		rejectPoor:       readInt64(fds.rejectPoor, buf),
		rejectReclaim:    readInt64(fds.rejectReclaim, buf),
		sameFilled:       readInt64(fds.sameFilled, buf),
		maxPoolPct:       readInt64(fds.maxPoolPct, buf),
		totalRAM:         getTotalRAM(fds.meminfo, buf),
		writebackEnabled: readBool(fds.writeback, buf),
		shrinkerEnabled:  readBool(fds.shrinker, buf),
	}
}

// ── formatting ──────────────────────────────────────────────────

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func fmtDelta(d int64) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("+%d", d)
}

// ── main ────────────────────────────────────────────────────────

func main() {
	fds := openZswapFds()
	defer fds.close()
	buf := make([]byte, 4096)

	initial := readStats(&fds, buf)
	startTime := time.Now()

	// Track min/max for each counter
	var (
		writtenBackMM minMax
		sameFilledMM  minMax
		rejectPoorMM  minMax
		rejectReclMM  minMax
	)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	render := func() {
		s := readStats(&fds, buf)

		// Update min/max trackers
		writtenBackMM.update(s.writtenBack)
		sameFilledMM.update(s.sameFilled)
		rejectPoorMM.update(s.rejectPoor)
		rejectReclMM.update(s.rejectReclaim)
		maxPool := s.totalRAM * s.maxPoolPct / 100
		storedBytes := s.storedPages * 4096

		var ratio float64
		if s.poolSize > 0 {
			ratio = float64(storedBytes) / float64(s.poolSize)
		}
		var usagePct float64
		if maxPool > 0 {
			usagePct = float64(s.poolSize) * 100 / float64(maxPool)
		}

		elapsed := time.Since(startTime).Truncate(time.Second)

		// Clear screen and move cursor home
		fmt.Print("\033[2J\033[H")

		fmt.Printf("zswap stats (refresh 1s, running %v)\n", elapsed)
		fmt.Println(strings.Repeat("─", 84))

		// Writeback/shrinker status (shrinker is modern replacement)
		wbStatus := "disabled"
		if s.shrinkerEnabled {
			wbStatus = "enabled (shrinker)"
		} else if s.writebackEnabled {
			wbStatus = "enabled (legacy)"
		}
		fmt.Printf("%-28s %s\n", "Writeback to swap:", wbStatus)
		fmt.Println()

		// Pool usage
		fmt.Printf("%-28s %8s / %s (%.1f%%)\n", "Pool RAM usage:",
			humanBytes(s.poolSize), humanBytes(maxPool), usagePct)
		fmt.Printf("%-28s %8s\n", "Stored (uncompressed):", humanBytes(storedBytes))
		fmt.Printf("%-28s %8.2fx\n", "Compression ratio:", ratio)
		fmt.Println()

		// Counters with lifetime, delta, min, max
		fmt.Printf("%-28s %12s %12s %12s %12s\n", "", "Lifetime", "Δ Session", "Min", "Max")
		fmt.Println(strings.Repeat("─", 84))

		fmt.Printf("%-28s %12d %12s %12d %12d\n", "Written back (pages):",
			s.writtenBack, fmtDelta(s.writtenBack-initial.writtenBack),
			writtenBackMM.min, writtenBackMM.max)
		fmt.Printf("%-28s %12d %12s %12d %12d\n", "Same-filled pages:",
			s.sameFilled, fmtDelta(s.sameFilled-initial.sameFilled),
			sameFilledMM.min, sameFilledMM.max)
		fmt.Printf("%-28s %12d %12s %12d %12d\n", "Reject (poor compress):",
			s.rejectPoor, fmtDelta(s.rejectPoor-initial.rejectPoor),
			rejectPoorMM.min, rejectPoorMM.max)
		fmt.Printf("%-28s %12d %12s %12d %12d\n", "Reject (reclaim fail):",
			s.rejectReclaim, fmtDelta(s.rejectReclaim-initial.rejectReclaim),
			rejectReclMM.min, rejectReclMM.max)

		canWriteback := s.writebackEnabled || s.shrinkerEnabled
		if !canWriteback && s.rejectReclaim > 0 {
			fmt.Println()
			fmt.Println("⚠ Writeback disabled - reclaim fails expected when pool fills")
		} else if canWriteback && s.rejectReclaim > initial.rejectReclaim {
			//fmt.Println()
			//fmt.Println("⚠ Reclaim fails during session - pressure exceeding writeback speed")
		}
	}

	render()
	for {
		select {
		case <-ticker.C:
			render()
		case <-sigCh:
			fmt.Print("\033[?25h") // show cursor
			fmt.Println("\nExiting.")
			return
		}
	}
}
