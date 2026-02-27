package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"psiparse"
)

const (
	sparkLen    = 60
	histLen     = sparkLen
	hourSamples = 3600
	barWidth    = 40
	pageKB      = 4 // x86_64 page size
	alpha       = 0.01
)

const (
	clearScr  = "\033[2J\033[H"
	hideCur   = "\033[?25l"
	showCur   = "\033[?25h"
	bold      = "\033[1m"
	dim       = "\033[2m"
	rst       = "\033[0m"
	fgRed     = "\033[91m"
	fgGreen   = "\033[92m"
	fgYellow  = "\033[93m"
	fgBlue    = "\033[94m"
	fgMagenta = "\033[95m"
	fgCyan    = "\033[96m"
)

var sparks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// --------------- 1-hour sliding ring buffer ---------------

type ringBuf struct {
	data [hourSamples]float64
	pos  int
	n    int
}

func (r *ringBuf) add(v float64) {
	r.data[r.pos] = v
	r.pos = (r.pos + 1) % hourSamples
	if r.n < hourSamples {
		r.n++
	}
}

func (r *ringBuf) buildSketch() *ddsketch.DDSketch {
	sk, _ := ddsketch.NewDefaultDDSketch(alpha)
	if r.n < hourSamples {
		for i := 0; i < r.n; i++ {
			sk.Add(r.data[i])
		}
	} else {
		for i := 0; i < hourSamples; i++ {
			sk.Add(r.data[(r.pos+i)%hourSamples])
		}
	}
	return sk
}

func (r *ringBuf) lastN(n int) []float64 {
	if n > r.n {
		n = r.n
	}
	out := make([]float64, n)
	start := r.pos - n
	if start < 0 {
		start += hourSamples
	}
	for i := 0; i < n; i++ {
		out[i] = r.data[(start+i)%hourSamples]
	}
	return out
}

func getQuantile(sk *ddsketch.DDSketch, q float64) (float64, bool) {
	if sk.GetCount() == 0 {
		return 0, false
	}
	v, err := sk.GetValueAtQuantile(q)
	if err != nil {
		return 0, false
	}
	return v, true
}

func fmtDuration(secs int) string {
	if secs >= 3600 {
		return fmt.Sprintf("%dh %dm", secs/3600, (secs%3600)/60)
	}
	if secs >= 60 {
		return fmt.Sprintf("%dm %ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%ds", secs)
}

type memStats struct {
	memAvail   uint64 // all in KB
	swapTotal  uint64
	swapFree   uint64
	swapCached uint64
	zswapped   uint64 // Zswapped: uncompressed size living in zswap pool
}

func (m memStats) swapUsed() uint64 {
	if m.swapTotal <= m.swapFree {
		return 0
	}
	return m.swapTotal - m.swapFree
}

// diskOnly returns swap data that is on disk and NOT in RAM.
// This is the "hard demand" — it excludes zswap (compressed in RAM)
// and SwapCached (on disk but clean copy already in page cache).
func (m memStats) diskOnly() uint64 {
	used := m.swapUsed()
	sub := m.swapCached + m.zswapped
	if sub >= used {
		return 0
	}
	return used - sub
}

func readMeminfo() memStats {
	var m memStats
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		flds := strings.Fields(s.Text())
		if len(flds) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(flds[1], 10, 64)
		switch flds[0] {
		case "MemAvailable:":
			m.memAvail = v
		case "SwapTotal:":
			m.swapTotal = v
		case "SwapFree:":
			m.swapFree = v
		case "SwapCached:":
			m.swapCached = v
		case "Zswapped:":
			m.zswapped = v
		}
	}
	return m
}

func readPswp() (uint64, uint64) {
	var in, out uint64
	f, err := os.Open("/proc/vmstat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		flds := strings.Fields(s.Text())
		if len(flds) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(flds[1], 10, 64)
		switch flds[0] {
		case "pswpin":
			in = v
		case "pswpout":
			out = v
		}
	}
	return in, out
}

// --------------- formatting helpers ---------------

func fmtKB(kb uint64) string {
	switch {
	case kb >= 1<<20:
		return fmt.Sprintf("%.1fG", float64(kb)/float64(uint64(1)<<20))
	case kb >= 1<<10:
		return fmt.Sprintf("%.0fM", float64(kb)/float64(uint64(1)<<10))
	default:
		return fmt.Sprintf("%dK", kb)
	}
}

func fmtRate(kbps float64) string {
	switch {
	case kbps >= float64(uint64(1)<<20):
		return fmt.Sprintf("%.1f G/s", kbps/float64(uint64(1)<<20))
	case kbps >= float64(uint64(1)<<10):
		return fmt.Sprintf("%.1f M/s", kbps/float64(uint64(1)<<10))
	default:
		return fmt.Sprintf("%.0f K/s", kbps)
	}
}

func fmtPSI(v float64) string { return fmt.Sprintf("%.2f%%", v) }

func makeBar(frac float64, w int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac * float64(w))
	var b strings.Builder
	b.Grow(w * 3)
	for i := 0; i < w; i++ {
		if i < n {
			b.WriteRune('\u2588') // █
		} else {
			b.WriteRune('\u2591') // ░
		}
	}
	return b.String()
}

func sparkline(hist []float64, maxVal float64) string {
	if maxVal <= 0 {
		maxVal = 1
	}
	var b strings.Builder
	// left-pad so the line is always histLen wide
	for i := len(hist); i < histLen; i++ {
		b.WriteRune(' ')
	}
	for _, v := range hist {
		idx := int(v / maxVal * float64(len(sparks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparks) {
			idx = len(sparks) - 1
		}
		b.WriteRune(sparks[idx])
	}
	return b.String()
}

func peakOf(s []float64) float64 {
	var m float64
	for _, v := range s {
		if v > m {
			m = v
		}
	}
	return m
}

// --------------- pressure assessment ---------------

func assessPressure(si, so float64, mem memStats) (string, string) {
	if mem.swapTotal == 0 {
		return "NO SWAP — none configured", fgCyan
	}
	if mem.swapUsed() == 0 && si == 0 && so == 0 {
		return "IDLE — no swap in use", fgGreen
	}

	switch {
	case si > 10240 && so > 10240:
		return "THRASHING — heavy bidirectional swap I/O", bold + fgRed
	case so > 10240:
		return "HEAVY EVICTION — rapidly pushing pages to swap", bold + fgRed
	case so > 1024:
		return "EVICTING — actively writing pages to swap", fgRed
	case si > 10240:
		return "HEAVY FAULT-IN — rapidly reading pages from swap", fgYellow
	case si > 1024:
		return "FAULTING — reading pages back from swap", fgYellow
	case si > 0 || so > 0:
		return "LIGHT I/O — minor swap activity", fgYellow
	default:
		disk := mem.diskOnly()
		if disk == 0 {
			return "PAST PRESSURE — swap slots allocated but all data in RAM", fgGreen
		}
		pct := float64(disk) / float64(mem.swapTotal) * 100
		if pct < 5 {
			return "PAST PRESSURE — negligible disk swap residue", fgGreen
		}
		return "PAST PRESSURE — swap on disk but idle, no active paging", fgGreen
	}
}

// --------------- swpd trend ---------------

func swpdTrend(hist []uint64) string {
	n := len(hist)
	if n < 5 {
		return dim + "collecting..." + rst
	}
	// compare last value to value 10s ago (or oldest if < 10 samples)
	lookback := 10
	if lookback > n-1 {
		lookback = n - 1
	}
	prev := hist[n-1-lookback]
	cur := hist[n-1]

	delta := int64(cur) - int64(prev)
	threshold := int64(1024) // 1 MB noise floor
	switch {
	case delta > threshold:
		return fgRed + "GROWING " + rst + dim + "(+" + fmtKB(uint64(delta)) + " over " +
			fmt.Sprintf("%ds", lookback) + ")" + rst
	case delta < -threshold:
		return fgGreen + "SHRINKING " + rst + dim + "(-" + fmtKB(uint64(-delta)) + " over " +
			fmt.Sprintf("%ds", lookback) + ")" + rst
	default:
		return fgGreen + "STABLE " + rst + dim + "(~" + fmtKB(cur) + " for " +
			fmt.Sprintf("%ds", lookback) + ")" + rst
	}
}

// --------------- main ---------------

func main() {
	fmt.Print(hideCur)
	defer fmt.Print(showCur + "\n")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	siSketch, _ := ddsketch.NewDefaultDDSketch(alpha)
	soSketch, _ := ddsketch.NewDefaultDDSketch(alpha)
	psiSomeSketch, _ := ddsketch.NewDefaultDDSketch(alpha)
	psiFullSketch, _ := ddsketch.NewDefaultDDSketch(alpha)
	sampleCount := 0

	psiFd, err := syscall.Open("/proc/pressure/memory", syscall.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open /proc/pressure/memory: %v\n", err)
		os.Exit(1)
	}
	defer syscall.Close(psiFd)
	var psiBuf [512]byte

	var (
		prevIn, prevOut uint64
		first           = true
		siHist          []float64
		soHist          []float64
		swpdHist        []uint64
	)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		mem := readMeminfo()
		curIn, curOut := readPswp()

		if first {
			prevIn, prevOut = curIn, curOut
			first = false
			fmt.Print(clearScr)
			fmt.Printf("\n  %s%sSwap Pressure Monitor%s  %swaiting for first sample...%s\n",
				bold, fgCyan, rst, dim, rst)
			select {
			case <-tick.C:
				continue
			case <-sig:
				return
			}
		}

		siRate := float64((curIn - prevIn) * pageKB)
		soRate := float64((curOut - prevOut) * pageKB)
		prevIn, prevOut = curIn, curOut
		sampleCount++

		siSketch.Add(siRate)
		soSketch.Add(soRate)

		psiSome, psiFull := psiparse.Read(psiFd, psiBuf[:])
		psiSomeSketch.Add(psiSome.Avg10)
		psiFullSketch.Add(psiFull.Avg10)

		siHist = append(siHist, siRate)
		soHist = append(soHist, soRate)
		swpdHist = append(swpdHist, mem.diskOnly())
		if len(siHist) > histLen {
			siHist = siHist[len(siHist)-histLen:]
			soHist = soHist[len(soHist)-histLen:]
			swpdHist = swpdHist[len(swpdHist)-histLen:]
		}

		peak := peakOf(siHist)
		if m := peakOf(soHist); m > peak {
			peak = m
		}
		if peak < 1024 {
			peak = 1024 // minimum 1 M/s scale
		}

		// ---- render ----
		var b strings.Builder

		b.WriteString(clearScr)
		fmt.Fprintf(&b, "\n  %s%sSwap Pressure Monitor%s  %s1s interval | ^C quit%s\n\n",
			bold, fgCyan, rst, dim, rst)

		// swap usage — disk-only is the primary metric
		disk := mem.diskOnly()
		pct := 0.0
		if mem.swapTotal > 0 {
			pct = float64(disk) / float64(mem.swapTotal) * 100
		}
		barColor := fgGreen
		if pct > 80 {
			barColor = fgRed
		} else if pct > 50 {
			barColor = fgYellow
		}
		fmt.Fprintf(&b, "  Swap:%7s / %-7s  %s%s%s  %4.1f%%  %s(disk-only)%s\n",
			fmtKB(disk), fmtKB(mem.swapTotal),
			barColor, makeBar(pct/100, barWidth), rst, pct, dim, rst)
		fmt.Fprintf(&b, "  %szswap:%s  %-8s %s\n  cached:%s %-8s %sRAM avail:%s %s\n\n",
			dim, rst, fmtKB(mem.zswapped),
			dim, rst, fmtKB(mem.swapCached),
			dim, rst, fmtKB(mem.memAvail))

		// status verdict
		status, color := assessPressure(siRate, soRate, mem)
		fmt.Fprintf(&b, "  %s●  %s%s\n\n", color, status, rst)

		// current rates
		fmt.Fprintf(&b, "  %ssi%s (from swap):  %9s  %s%s%s\n",
			bold, rst, fmtRate(siRate), fgBlue, makeBar(siRate/peak, barWidth), rst)
		fmt.Fprintf(&b, "  %sso%s (to swap):    %9s  %s%s%s\n\n",
			bold, rst, fmtRate(soRate), fgRed, makeBar(soRate/peak, barWidth), rst)

		// DDSketch percentiles
		fmt.Fprintf(&b, "  %sDDSketch%s %s(%d samples, %s):%s\n",
			bold, rst, dim, sampleCount, fmtDuration(sampleCount), rst)
		fmt.Fprintf(&b, "            %10s %10s %10s\n", "p50", "p90", "p99")
		siP50, _ := getQuantile(siSketch, 0.50)
		siP90, _ := getQuantile(siSketch, 0.90)
		siP99, _ := getQuantile(siSketch, 0.99)
		soP50, _ := getQuantile(soSketch, 0.50)
		soP90, _ := getQuantile(soSketch, 0.90)
		soP99, _ := getQuantile(soSketch, 0.99)
		fmt.Fprintf(&b, "  %ssi%s       %10s %10s %10s\n", bold, rst, fmtRate(siP50), fmtRate(siP90), fmtRate(siP99))
		fmt.Fprintf(&b, "  %sso%s       %10s %10s %10s\n", bold, rst, fmtRate(soP50), fmtRate(soP90), fmtRate(soP99))
		psP50, _ := getQuantile(psiSomeSketch, 0.50)
		psP90, _ := getQuantile(psiSomeSketch, 0.90)
		psP99, _ := getQuantile(psiSomeSketch, 0.99)
		pfP50, _ := getQuantile(psiFullSketch, 0.50)
		pfP90, _ := getQuantile(psiFullSketch, 0.90)
		pfP99, _ := getQuantile(psiFullSketch, 0.99)
		fmt.Fprintf(&b, "  %spsi some%s %10s %10s %10s\n", bold, rst, fmtPSI(psP50), fmtPSI(psP90), fmtPSI(psP99))
		fmt.Fprintf(&b, "  %spsi full%s %10s %10s %10s\n\n", bold, rst, fmtPSI(pfP50), fmtPSI(pfP90), fmtPSI(pfP99))

		// sparkline history
		fmt.Fprintf(&b, "  %sHistory%s %s(%ds, peak %s):%s\n",
			bold, rst, dim, len(siHist), fmtRate(peak), rst)
		fmt.Fprintf(&b, "  %ssi%s %s%s%s\n", bold, rst, fgBlue, sparkline(siHist, peak), rst)
		fmt.Fprintf(&b, "  %sso%s %s%s%s\n\n", bold, rst, fgRed, sparkline(soHist, peak), rst)

		// swpd trend
		fmt.Fprintf(&b, "  %sswpd:%s %s\n", bold, rst, swpdTrend(swpdHist))

		fmt.Print(b.String())

		select {
		case <-tick.C:
		case <-sig:
			return
		}
	}
}
