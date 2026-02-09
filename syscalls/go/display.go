package main

import (
	"fmt"
	"strings"
	"time"
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

const summaryLineWidth = 97

// Display handles rendering
type Display struct {
	batchMode      bool
	focusProcesses []string // ordered list of focus process names
	topN           int
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

// buildSepLine builds "== title ====...==== legend ==" fitting within width.
func buildSepLine(width int, title, legend string) string {
	prefix := "== " + title + " "
	var suffix string
	if legend != "" {
		suffix = " " + legend + " =="
	}
	fill := width - len(prefix) - len(suffix)
	if fill < 0 {
		fill = 0
	}
	return prefix + strings.Repeat("=", fill) + suffix
}

// summaryBarLegend returns the legend text to embed in the summary bar.
func (d *Display) summaryBarLegend() string {
	if !d.interactive {
		return ""
	}
	switch d.mode {
	case modeNormal:
		return "[/] filter  [q] quit"
	case modeFilter:
		return fmt.Sprintf("Filter: %s_  [/] cancel  [Bksp] back", d.filterText)
	}
	return ""
}

func (d *Display) render(state *State, metrics *runtimeMetrics, mapCap int64, rs *ringStats) {
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
		d.renderSummary(&mainBuf, viewStats, elapsed, state.globalStats, sketchPercentiles(state.globalSketch))
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
	d.renderFooter(&footerBuf, elapsed, totalSamples, nProcs, metrics, mapCap, rs)

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
		panelMaxRows := d.topN + 1
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

	// Mode hints and filter prompt (only for table view; summary view embeds in summary bar)
	if d.interactive && len(d.focusProcesses) > 0 {
		switch d.mode {
		case modeNormal:
			output.WriteString("  [/] filter  [q] quit\n")
		case modeFilter:
			fmt.Fprintf(&output, "  Filter prefix (proc/syscall): %s_  [/] cancel  [Bksp] back\n", d.filterText)
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

func (d *Display) renderFooter(buf *strings.Builder, elapsed time.Duration, totalSamples uint64, nProcs int, metrics *runtimeMetrics, mapCap int64, rs *ringStats) {
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
	if rs != nil {
		pctFull := float64(rs.pending) / float64(rs.capBytes) * 100
		maxPct := float64(rs.maxPending) / float64(rs.capBytes) * 100
		last0Str := "-"
		if rs.last0 > 0 {
			last0Str = formatMicro(rs.last0)
		}
		ringInfo = fmt.Sprintf(" | Ring avg: %6s/%s (%5.1f%%)  Ring max: %6s/%s (%5.1f%%)  avg1:%-6.0f avg0:%-8.1f last1:%-6s last0:%-8s",
			formatBytes(int64(rs.pending)), formatBytes(int64(rs.capBytes)), pctFull,
			formatBytes(rs.maxPending), formatBytes(int64(rs.capBytes)), maxPct,
			rs.avg1, rs.avg0, formatCount(rs.last1), last0Str)
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
		renderDetailRow(buf, name, e.ss.stats, sketchPercentiles(e.ss.sketch), e.ss.top)
	}

	buf.WriteString("\n")
}

func renderDetailRow(buf *strings.Builder, name string, st *simpleStats, pcts percentiles, top *topN) {
	n := st.count
	if n == 0 {
		fmt.Fprintf(buf, "%s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
			name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
		return
	}
	fmt.Fprintf(buf, "%s │ %s %s %s %s %s %s %s %s %s │ %s │ %9s\n",
		name,
		formatLatencyPadded(st.min),
		formatLatencyPadded(st.Avg()),
		formatLatencyPadded(pcts.P25),
		formatLatencyPadded(pcts.P50),
		formatLatencyPadded(pcts.P75),
		formatLatencyPadded(pcts.P90),
		formatLatencyPadded(pcts.P99),
		formatLatencyPadded(pcts.P999),
		formatLatencyPadded(st.max),
		formatTop5(top),
		formatCount(int64(n)),
	)
}

func (d *Display) renderSummary(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats, elapsed time.Duration, globalStats *simpleStats, globalPcts percentiles) {
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

	// Section header is embedded in the summary bar (=== line) instead of a separate line.

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
			leftStr = formatSummaryRow(leftSlice[i].label, leftSlice[i].ss.stats, sketchPercentiles(leftSlice[i].ss.sketch), totalSecs)
		} else {
			leftStr = strings.Repeat(" ", summaryLineWidth)
		}

		if i < len(rightSlice) {
			rightStr = formatSummaryRow(rightSlice[i].label, rightSlice[i].ss.stats, sketchPercentiles(rightSlice[i].ss.sketch), totalSecs)
		} else {
			rightStr = strings.Repeat(" ", summaryLineWidth)
		}

		fmt.Fprintf(buf, "%s │ %s\n", leftStr, rightStr)
	}

	// Build summary bar: LIFETIME(all) stats == title == legend ==
	title := fmt.Sprintf("Process × Syscall (top %d)", totalShown)
	legend := d.summaryBarLegend()

	if globalStats.count > 0 {
		globalRow := formatSummaryRow("LIFETIME(all)", globalStats, globalPcts, totalSecs)
		remaining := dualWidth - summaryLineWidth - 1
		if remaining < 0 {
			remaining = 0
		}
		fmt.Fprintf(buf, "%s %s\n", globalRow, buildSepLine(remaining, title, legend))
	} else {
		buf.WriteString(buildSepLine(dualWidth, title, legend))
		buf.WriteString("\n")
	}
}

func formatSummaryRow(name string, st *simpleStats, pcts percentiles, secs float64) string {
	n := st.count
	if n == 0 {
		return fmt.Sprintf("%-28s │ %8s %8s %8s %8s %8s │ %9s %9s",
			name, "-", "-", "-", "-", "-", "0", "-")
	}
	return fmt.Sprintf("%-28s │ %s %s %s %s %s │ %9s %9s",
		name,
		formatLatencyPadded(st.Avg()),
		formatLatencyPadded(pcts.P50),
		formatLatencyPadded(pcts.P90),
		formatLatencyPadded(pcts.P99),
		formatLatencyPadded(st.max),
		formatCount(int64(n)),
		formatRate(n, secs),
	)
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
