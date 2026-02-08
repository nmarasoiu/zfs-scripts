package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/nmarasoiu/zfs-scripts/ringpoll"
)

const (
	panelWidth      = 34
	panelSep        = " │ "
	panelSepDisplay = 3 // display width of panelSep (│ is 1 column wide)
	panelOverhead   = panelSepDisplay + panelWidth
)

type interactiveMode int

const (
	modeNormal interactiveMode = iota
	modeFilter
)

type processSummary struct {
	name  string
	count uint64
	rate  float64
}

type tableEntry struct {
	label string
	ss    *syscallStats
}

const summaryLineWidth = 97

// Display handles rendering
type Display struct {
	batchMode      bool
	focusProcesses []string // ordered list of focus process names
	topN           int
	ring           *ringpoll.Reader
	colsOverride   int // --cols override; 0 = auto-detect

	// Interactive state (all owned by display goroutine — no sync needed)
	interactive   bool
	mode          interactiveMode
	filterText    string
	lastSummaries []processSummary
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
}

func formatTop5(top *topN) string {
	vals := top.Get()
	var parts []string
	for i := 0; i < 5-len(vals); i++ {
		parts = append(parts, fmt.Sprintf("%8s", "-"))
	}
	for _, v := range vals {
		parts = append(parts, formatLatencyPadded(v))
	}
	return strings.Join(parts, " ")
}

func sectionHeader(buf *strings.Builder, title string, width int) {
	displayWidth := 3 + len(title) + 1
	remaining := width - displayWidth
	if remaining < 0 {
		remaining = 0
	}
	buf.WriteString("── ")
	buf.WriteString(title)
	buf.WriteString(" ")
	buf.WriteString(strings.Repeat("─", remaining))
	buf.WriteString("\n")
}

// collectEntries builds a sorted list of table entries from per-process stats.
// When alwaysPrefix is true, labels are always "proc/syscall"; otherwise the
// proc prefix is omitted when there is exactly one process.
func collectEntries(procStats map[string]map[uint32]*syscallStats, alwaysPrefix bool) []tableEntry {
	singleProc := !alwaysPrefix && len(procStats) == 1

	var entries []tableEntry
	for proc, fm := range procStats {
		for id, ss := range fm {
			label := syscallName(id)
			if !singleProc {
				label = proc + "/" + label
			}
			entries = append(entries, tableEntry{label, ss})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		ci := entries[i].ss.stats.count
		cj := entries[j].ss.stats.count
		if ci != cj {
			return ci > cj
		}
		return entries[i].label < entries[j].label
	})

	return entries
}

func (d *Display) render(state *State, metrics *runtimeMetrics, mapCap int64) {
	var mainBuf strings.Builder
	now := time.Now()

	state.mu.Lock()

	elapsed := now.Sub(state.startTime)

	// Count total sketches (one per process×syscall pair)
	nSketches := 0
	for _, fm := range state.procSyscallStats {
		nSketches += len(fm)
	}

	const sketchBytesEach = 400 // empirical ~0.4KB per DDSketch (struct + mapping + stores + few bins)
	totalMB := float64(nSketches) * sketchBytesEach / 1024 / 1024

	fmt.Fprintf(&mainBuf, "Syscall Latency Monitor - %s (uptime: %s) -- %d sketches × 0.4KB ≈ %.1fMB\n",
		now.Format("15:04:05"), formatDuration(elapsed), nSketches, totalMB)

	// Collect process summaries for the panel (while holding lock)
	d.lastSummaries = collectProcessSummaries(state.procSyscallStats, elapsed.Seconds())

	// Optionally filter procSyscallStats for display
	viewStats := state.procSyscallStats
	if d.mode == modeFilter && d.filterText != "" {
		viewStats = filterStatsGeneral(viewStats, d.filterText)
	}

	// Main content
	if len(d.focusProcesses) > 0 {
		d.renderTable(&mainBuf, viewStats)
	} else {
		d.renderSummary(&mainBuf, viewStats, elapsed)
	}

	// Totals for footer
	var totalSamples uint64
	for _, fm := range state.procSyscallStats {
		for _, ss := range fm {
			totalSamples += ss.stats.count
		}
	}
	nProcs := len(state.procSyscallStats)

	state.mu.Unlock()

	// Build footer
	var footerBuf strings.Builder
	d.renderFooter(&footerBuf, elapsed, totalSamples, nProcs, metrics, mapCap)

	// Compose main content with panel when terminal is wide enough.
	// Panel display is independent of interactive mode — --cols enables it in batch too.
	mainStr := mainBuf.String()
	footerStr := footerBuf.String()
	termW, _ := termSize()
	if d.colsOverride > 0 {
		termW = d.colsOverride
	}

	mainLines := strings.Split(strings.TrimRight(mainStr, "\n"), "\n")

	// Compute max display width of main content to decide if panel fits.
	maxContentWidth := 0
	for _, line := range mainLines {
		if w := displayWidth(line); w > maxContentWidth {
			maxContentWidth = w
		}
	}
	showPanel := termW >= maxContentWidth+panelOverhead

	var output strings.Builder

	if showPanel {
		panelMaxRows := d.topN + 2
		var panelMatchedProcs map[string]bool
		if d.mode == modeFilter && d.filterText != "" {
			panelMatchedProcs = make(map[string]bool, len(viewStats))
			for proc := range viewStats {
				panelMatchedProcs[proc] = true
			}
		}
		panelLines := renderPanel(d.lastSummaries, panelMatchedProcs, panelMaxRows)

		// mainColWidth = space allocated to main content (display columns).
		mainColWidth := termW - panelOverhead

		for i := 0; i < len(mainLines); i++ {
			left := mainLines[i]
			displayLen := displayWidth(left)
			if displayLen < mainColWidth {
				left += strings.Repeat(" ", mainColWidth-displayLen)
			}
			right := ""
			if i < len(panelLines) {
				right = panelLines[i]
			}
			if right != "" {
				fmt.Fprintf(&output, "%s%s%s\n", left, panelSep, padOrTrunc(right, panelWidth))
			} else {
				output.WriteString(left)
				output.WriteByte('\n')
			}
		}
	} else {
		output.WriteString(mainStr)
	}

	output.WriteString(footerStr)

	// Mode hints and filter prompt
	if d.interactive {
		switch d.mode {
		case modeNormal:
			output.WriteString("  [/] filter  [q] quit\n")
		case modeFilter:
			fmt.Fprintf(&output, "  Filter (proc/syscall): %s_  [/] cancel  [Bksp] back\n", d.filterText)
		}
	}

	d.resetCursor()
	fmt.Print(output.String())
}

// handleKey processes a key event and updates display mode/state.
// Returns true if the program should quit.
func (d *Display) handleKey(ev keyEvent) bool {
	switch d.mode {
	case modeNormal:
		if ev.kind == keyChar {
			switch ev.ch {
			case '/':
				d.mode = modeFilter
				d.filterText = ""
			case 'q':
				return true
			}
		}
	case modeFilter:
		switch ev.kind {
		case keyChar:
			if ev.ch == '/' {
				d.filterText = ""
				d.mode = modeNormal
			} else {
				d.filterText += string(ev.ch)
			}
		case keyBackspace:
			if len(d.filterText) > 0 {
				d.filterText = d.filterText[:len(d.filterText)-1]
			} else {
				d.mode = modeNormal
			}
		}
	}
	return false
}

func (d *Display) renderFooter(buf *strings.Builder, elapsed time.Duration, totalSamples uint64, nProcs int, metrics *runtimeMetrics, mapCap int64) {
	drops := metrics.drops.Load()
	evicted := metrics.evicted.Load()
	mapUsed := metrics.mapUsed.Load()
	mapStale := metrics.mapStale.Load()

	rate := float64(0)
	if elapsed.Seconds() > 0 {
		rate = float64(totalSamples) / elapsed.Seconds()
	}
	dropRate := float64(0)
	if elapsed.Seconds() > 0 {
		dropRate = float64(drops) / elapsed.Seconds()
	}
	ringInfo := ""
	if d.ring != nil {
		pending := d.ring.Pending()
		capBytes := d.ring.BufSize()
		maxPend := d.ring.MaxPending()
		pctFull := float64(pending) / float64(capBytes) * 100
		maxPct := float64(maxPend) / float64(capBytes) * 100
		avg1, avg0, last1, last0 := d.ring.PollStats()
		last0Str := "-"
		if last0 > 0 {
			last0Str = formatMicro(last0)
		}
		ringInfo = fmt.Sprintf(" | Ring avg: %6s/%s (%5.1f%%)  Ring max: %6s/%s (%5.1f%%)  avg1:%-6.0f avg0:%-8.1f last1:%-6s last0:%-8s",
			formatBytes(int64(pending)), formatBytes(int64(capBytes)), pctFull,
			formatBytes(maxPend), formatBytes(int64(capBytes)), maxPct,
			avg1, avg0, formatCount(last1), last0Str)
	}
	mapInfo := ""
	if mapCap > 0 {
		pct := float64(mapUsed) / float64(mapCap) * 100
		mapInfo = fmt.Sprintf(" | Map: %s/%s (%4.1f%%) stale:%s evict:%s",
			formatCount(mapUsed), formatCount(mapCap), pct,
			formatCount(mapStale), formatCount(int64(evicted)))
	}
	fmt.Fprintf(buf, "Total: %s syscalls | Rate: %s/s | Processes: %d | Drops: %s (%s/s)%s%s\n",
		formatCount(int64(totalSamples)), formatCount(int64(rate)), nProcs,
		formatCount(int64(drops)), formatCount(int64(dropRate)), mapInfo, ringInfo)

	if d.batchMode {
		buf.WriteString("\n")
	}
}

func (d *Display) renderTable(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats) {
	entries := collectEntries(procStats, false)

	if len(entries) == 0 {
		return
	}

	// Limit to top N (0 = all)
	shown := len(entries)
	if d.topN > 0 && shown > d.topN {
		shown = d.topN
	}

	// Compute label column width from visible entries
	labelWidth := 12
	for i := 0; i < shown; i++ {
		if n := len(entries[i].label); n > labelWidth {
			labelWidth = n
		}
	}
	labelWidth++ // padding

	// Section title
	var title string
	if len(d.focusProcesses) > 0 {
		title = strings.Join(d.focusProcesses, ",")
	} else {
		title = "All Processes"
	}
	lineWidth := labelWidth + 142
	sectionHeader(buf, fmt.Sprintf("%s (%d)", title, shown), lineWidth)

	// Column headers
	nameFmt := fmt.Sprintf("%%-%ds", labelWidth)
	fmt.Fprintf(buf, "%s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
		fmt.Sprintf(nameFmt, "LIFETIME"),
		"min", "avg", "p25", "p50", "p75", "p90", "p99", "p99.9", "max",
		"max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Data rows
	for i := 0; i < shown; i++ {
		e := entries[i]
		name := fmt.Sprintf(nameFmt, e.label)
		renderDetailRow(buf, name, e.ss.stats, e.ss.sketch, e.ss.top)
	}

	buf.WriteString("\n")
}

func renderDetailRow(buf *strings.Builder, name string, st *simpleStats, sketch *ddsketch.DDSketch, top *topN) {
	n := st.count
	if n == 0 {
		fmt.Fprintf(buf, "%s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
			name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
		return
	}
	p25, _ := sketch.GetValueAtQuantile(0.25)
	p50, _ := sketch.GetValueAtQuantile(0.50)
	p75, _ := sketch.GetValueAtQuantile(0.75)
	p90, _ := sketch.GetValueAtQuantile(0.90)
	p99, _ := sketch.GetValueAtQuantile(0.99)
	p999, _ := sketch.GetValueAtQuantile(0.999)
	fmt.Fprintf(buf, "%s │ %s %s %s %s %s %s %s %s %s │ %s │ %9s\n",
		name,
		formatLatencyPadded(st.min),
		formatLatencyPadded(st.Avg()),
		formatLatencyPadded(int64(p25)),
		formatLatencyPadded(int64(p50)),
		formatLatencyPadded(int64(p75)),
		formatLatencyPadded(int64(p90)),
		formatLatencyPadded(int64(p99)),
		formatLatencyPadded(int64(p999)),
		formatLatencyPadded(st.max),
		formatTop5(top),
		formatCount(int64(n)),
	)
}

func (d *Display) renderSummary(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats, elapsed time.Duration) {
	entries := collectEntries(procStats, true)

	totalSecs := elapsed.Seconds()
	nPerCol := d.topN // rows per column
	if nPerCol <= 0 {
		nPerCol = (len(entries) + 1) / 2
	}
	totalShown := nPerCol * 2
	if totalShown > len(entries) {
		totalShown = len(entries)
	}

	dualWidth := summaryLineWidth + 3 + summaryLineWidth

	sectionHeader(buf, fmt.Sprintf("Process × Syscall (top %d)", totalShown), dualWidth)

	hdr := fmt.Sprintf("%-28s │ %8s %8s %8s %8s %8s │ %9s %9s",
		"LIFETIME", "avg", "p50", "p90", "p99", "max", "samples", "rate")
	fmt.Fprintf(buf, "%s │ %s\n", hdr, hdr)
	buf.WriteString(strings.Repeat("-", dualWidth))
	buf.WriteString("\n")

	leftEnd := nPerCol
	if leftEnd > len(entries) {
		leftEnd = len(entries)
	}
	rightStart := nPerCol
	rightEnd := nPerCol * 2
	if rightEnd > len(entries) {
		rightEnd = len(entries)
	}

	leftSlice := entries[:leftEnd]
	var rightSlice []tableEntry
	if rightStart < len(entries) {
		rightSlice = entries[rightStart:rightEnd]
	}

	maxRows := len(leftSlice)
	if len(rightSlice) > maxRows {
		maxRows = len(rightSlice)
	}

	for i := 0; i < maxRows; i++ {
		var leftStr, rightStr string

		if i < len(leftSlice) {
			leftStr = formatSummaryRow(leftSlice[i].label, leftSlice[i].ss.stats, leftSlice[i].ss.sketch, totalSecs)
		} else {
			leftStr = strings.Repeat(" ", summaryLineWidth)
		}

		if i < len(rightSlice) {
			rightStr = formatSummaryRow(rightSlice[i].label, rightSlice[i].ss.stats, rightSlice[i].ss.sketch, totalSecs)
		} else {
			rightStr = strings.Repeat(" ", summaryLineWidth)
		}

		fmt.Fprintf(buf, "%s │ %s\n", leftStr, rightStr)
	}

	buf.WriteString(strings.Repeat("=", dualWidth))
	buf.WriteString("\n")
}

func formatSummaryRow(name string, st *simpleStats, sketch *ddsketch.DDSketch, secs float64) string {
	n := st.count
	if n == 0 {
		return fmt.Sprintf("%-28s │ %8s %8s %8s %8s %8s │ %9s %9s",
			name, "-", "-", "-", "-", "-", "0", "-")
	}
	p50, _ := sketch.GetValueAtQuantile(0.50)
	p90, _ := sketch.GetValueAtQuantile(0.90)
	p99, _ := sketch.GetValueAtQuantile(0.99)
	return fmt.Sprintf("%-28s │ %s %s %s %s %s │ %9s %9s",
		name,
		formatLatencyPadded(st.Avg()),
		formatLatencyPadded(int64(p50)),
		formatLatencyPadded(int64(p90)),
		formatLatencyPadded(int64(p99)),
		formatLatencyPadded(st.max),
		formatCount(int64(n)),
		formatRate(n, secs),
	)
}

// filterStatsGeneral returns a filtered copy of procStats where entries match
// the text against process name or syscall name (case-insensitive substring).
// Process name matches include all syscalls; syscall matches are per-entry.
// Must be called while state.mu is held.
func filterStatsGeneral(procStats map[string]map[uint32]*syscallStats, text string) map[string]map[uint32]*syscallStats {
	lower := strings.ToLower(text)
	filtered := make(map[string]map[uint32]*syscallStats)
	for proc, fm := range procStats {
		if strings.Contains(strings.ToLower(proc), lower) {
			filtered[proc] = fm
			continue
		}
		matched := make(map[uint32]*syscallStats)
		for id, ss := range fm {
			if strings.Contains(syscallName(id), lower) {
				matched[id] = ss
			}
		}
		if len(matched) > 0 {
			filtered[proc] = matched
		}
	}
	return filtered
}

// collectProcessSummaries aggregates per-process totals from procSyscallStats.
// Must be called while state.mu is held.
func collectProcessSummaries(procStats map[string]map[uint32]*syscallStats, elapsedSecs float64) []processSummary {
	summaries := make([]processSummary, 0, len(procStats))
	for proc, fm := range procStats {
		var total uint64
		for _, ss := range fm {
			total += ss.stats.count
		}
		rate := float64(0)
		if elapsedSecs > 0 {
			rate = float64(total) / elapsedSecs
		}
		summaries = append(summaries, processSummary{name: proc, count: total, rate: rate})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].count != summaries[j].count {
			return summaries[i].count > summaries[j].count
		}
		return summaries[i].name < summaries[j].name
	})
	return summaries
}

// displayWidth returns the number of display columns a string occupies.
// Counts each rune as 1 column (works for ASCII + box-drawing chars).
func displayWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] < 0x80 {
			n++
			i++
		} else {
			// Skip continuation bytes of multi-byte UTF-8 sequence
			// The lead byte counts as 1 display column
			n++
			i++
			for i < len(s) && s[i]&0xC0 == 0x80 {
				i++
			}
		}
	}
	return n
}

// padOrTrunc pads with spaces or truncates to exactly width display columns.
func padOrTrunc(s string, width int) string {
	dw := displayWidth(s)
	if dw >= width {
		// Truncate to width display columns
		n := 0
		i := 0
		for i < len(s) && n < width {
			if s[i] < 0x80 {
				i++
			} else {
				i++
				for i < len(s) && s[i]&0xC0 == 0x80 {
					i++
				}
			}
			n++
		}
		return s[:i]
	}
	return s + strings.Repeat(" ", width-dw)
}

// renderPanel builds the right-side process panel lines.
// maxRows limits the number of process rows (0 = unlimited).
func renderPanel(summaries []processSummary, matchedProcs map[string]bool, maxRows int) []string {
	var lines []string

	// Header
	lines = append(lines, padOrTrunc("  PROCESS         RATE    TOTAL", panelWidth))
	lines = append(lines, strings.Repeat("─", panelWidth))

	n := 0
	for _, ps := range summaries {
		if matchedProcs != nil && !matchedProcs[ps.name] {
			continue
		}
		if maxRows > 0 && n >= maxRows {
			break
		}
		rateStr := "-"
		if ps.rate >= 1 {
			rateStr = formatCount(int64(ps.rate)) + "/s"
		} else if ps.rate > 0 {
			rateStr = fmt.Sprintf("%.1f/s", ps.rate)
		}
		line := fmt.Sprintf("  %-15s %8s %8s", padOrTrunc(ps.name, 15), rateStr, formatCount(int64(ps.count)))
		lines = append(lines, padOrTrunc(line, panelWidth))
		n++
	}
	return lines
}
