package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// --- Display data transforms (turn State into renderable structures) ---

type processSummary struct {
	name  string
	count uint64
	rate  float64
}

type tableEntry struct {
	label string
	ss    *syscallStats
}

// collectEntries builds a sorted list of table entries from per-process stats.
// When singleProc is true, the proc prefix is omitted from labels.
func collectEntries(procStats map[string]map[uint32]*syscallStats, singleProc bool) []tableEntry {

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

// filterStatsGeneral returns a filtered copy of procStats where entries match
// the text against process name or syscall name (case-insensitive substring).
// Process name matches include all syscalls; syscall matches are per-entry.
func filterStatsGeneral(procStats map[string]map[uint32]*syscallStats, text string) map[string]map[uint32]*syscallStats {
	lower := strings.ToLower(text)
	filtered := make(map[string]map[uint32]*syscallStats)
	for proc, fm := range procStats {
		if strings.HasPrefix(strings.ToLower(proc), lower) {
			filtered[proc] = fm
			continue
		}
		matched := make(map[uint32]*syscallStats)
		for id, ss := range fm {
			if strings.HasPrefix(syscallName(id), lower) {
				matched[id] = ss
			}
		}
		if len(matched) > 0 {
			filtered[proc] = matched
		}
	}
	return filtered
}

// collectProcessSummaries aggregates per-process totals.
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

const (
	procPanelWidth      = 34
	procPanelSep        = " │ "
	procPanelSepDisplay = 3 // display width of procPanelSep (│ is 1 column wide)
	procPanelOverhead   = procPanelSepDisplay + procPanelWidth
)

type interactiveMode int

const (
	modeNormal interactiveMode = iota
	modeFilter
)

// Display handles rendering
type Display struct {
	batchMode      bool
	focusProcesses []string   // ordered list of focus process names
	topN           int
	colsOverride   int        // --cols override; 0 = auto-detect
	quantiles      []float64  // configured percentile quantiles (0.0–1.0)

	// Interactive state (all owned by display goroutine — no sync needed)
	interactive   bool
	mode          interactiveMode
	filterText    string
	lastSummaries []processSummary
}

// quantileHeader formats a quantile as a column header (e.g. 0.50 → "p50", 0.999 → "p99.9").
func quantileHeader(q float64) string {
	return fmt.Sprintf("p%g", q*100)
}

// summaryLineWidth computes the width of a summary row given the number of percentile columns.
// Layout: %-28s │ avg [pcts...] max │ samples rate
func summaryLineWidth(numPcts int) int {
	// 28 (name) + 3 (│) + (numPcts+2)*8 + (numPcts+1)*1 (value cols) + 3 (│) + 9+1+9 (samples+space+rate)
	return 28 + 3 + 9*numPcts + 17 + 3 + 19
}

// tableDataWidth computes the data area width (after label) for the table view.
// Layout: │ min avg [pcts...] max │ samples
func tableDataWidth(numPcts int) int {
	// 3 (│) + (numPcts+3)*8 + (numPcts+2)*1 (value cols) + 3 (│) + 9+1 (space+samples)
	return 9*numPcts + 41
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
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

func (d *Display) render(state *State, drops uint64, ms *mapStats, rs *ringStats) {
	var mainBuf strings.Builder
	var elapsed time.Duration
	var nProcs int
	var filterMatched map[string]bool // non-nil when filter is active

	state.Read(func(v StateView) {
		now := time.Now()
		elapsed = now.Sub(v.StartTime)

		const sketchBytesEach = 2048 // ~2KB per DDSketch: 0.7KB narrow, 4KB wide range (measured via protobuf + struct overhead)
		totalMB := float64(v.NSketches) * sketchBytesEach / 1024 / 1024

		fmt.Fprintf(&mainBuf, "Syscall Latency Monitor - %s (uptime: %s) -- %d sketches × 2KB ≈ %.1fMB  evict:%s\n",
			now.Format("15:04:05"), formatDuration(elapsed), v.NSketches, totalMB, formatCount(int64(v.SketchEvictions)))

		d.lastSummaries = collectProcessSummaries(v.ProcStats, elapsed.Seconds())

		viewStats := v.ProcStats
		if d.mode == modeFilter && d.filterText != "" {
			viewStats = filterStatsGeneral(viewStats, d.filterText)
			filterMatched = make(map[string]bool, len(viewStats))
			for proc := range viewStats {
				filterMatched[proc] = true
			}
		}

		if len(d.focusProcesses) > 0 {
			d.renderTable(&mainBuf, viewStats)
		} else {
			d.renderSummary(&mainBuf, viewStats, elapsed, v.GlobalStats, sketchPercentiles(v.GlobalSketch, d.quantiles))
		}

		nProcs = len(v.ProcStats)
	})

	// Build footer
	var footerBuf strings.Builder
	d.renderFooter(&footerBuf, elapsed, nProcs, drops, ms, rs)

	// Compose main content with top-processes panel when terminal is wide enough.
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
	showProcPanel := termW >= maxContentWidth+procPanelOverhead

	var output strings.Builder

	if showProcPanel {
		procPanelMaxRows := d.topN + 4
		procPanelLines := renderProcPanel(d.lastSummaries, filterMatched, procPanelMaxRows)

		// mainColWidth = space allocated to main content (display columns).
		mainColWidth := termW - procPanelOverhead

		for i := 0; i < len(mainLines); i++ {
			left := mainLines[i]
			displayLen := displayWidth(left)
			if displayLen < mainColWidth {
				left += strings.Repeat(" ", mainColWidth-displayLen)
			}
			right := ""
			if i < len(procPanelLines) {
				right = procPanelLines[i]
			}
			if right != "" {
				fmt.Fprintf(&output, "%s%s%s\n", left, procPanelSep, padOrTrunc(right, procPanelWidth))
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

func (d *Display) renderFooter(buf *strings.Builder, elapsed time.Duration, nProcs int, drops uint64, ms *mapStats, rs *ringStats) {
	dropRate := float64(0)
	if elapsed.Seconds() > 0 {
		dropRate = float64(drops) / elapsed.Seconds()
	}
	mapInfo := ""
	if ms != nil {
		mapInfo = fmt.Sprintf(" | Map %s", ms.formatUsage(formatCount))
	}
	ringInfo := ""
	if rs != nil {
		ringInfo = fmt.Sprintf(" | Ring %s  cur: %6s  avg1:%-6.0f avg0:%-8.1f last1:%-6s last0:%-8s",
			rs.formatUsage(formatBytes),
			formatBytes(int64(rs.pending)),
			rs.avg1, rs.avg0, formatCount(rs.last1), formatMicro(rs.last0))
	}
	fmt.Fprintf(buf, "Processes: %d | Drops: %s (%s/s)%s%s\n",
		nProcs, formatCount(int64(drops)), formatCount(int64(dropRate)), mapInfo, ringInfo)

	if d.batchMode {
		buf.WriteString("\n")
	}
}

func (d *Display) renderTable(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats) {
	entries := collectEntries(procStats, len(procStats) == 1)

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
	title := strings.Join(d.focusProcesses, ",")
	lineWidth := labelWidth + tableDataWidth(len(d.quantiles))
	sectionHeader(buf, fmt.Sprintf("%s (%d)", title, shown), lineWidth)

	// Column headers: LIFETIME │ min avg [pcts...] max │ samples
	nameFmt := fmt.Sprintf("%%-%ds", labelWidth)
	buf.WriteString(fmt.Sprintf(nameFmt, "LIFETIME"))
	buf.WriteString(" │")
	buf.WriteString(fmt.Sprintf(" %8s %8s", "min", "avg"))
	for _, q := range d.quantiles {
		fmt.Fprintf(buf, " %8s", quantileHeader(q))
	}
	fmt.Fprintf(buf, " %8s │ %9s\n", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Data rows
	for i := 0; i < shown; i++ {
		e := entries[i]
		name := fmt.Sprintf(nameFmt, e.label)
		renderDetailRow(buf, name, e.ss.stats, sketchPercentiles(e.ss.sketch, d.quantiles))
	}

	buf.WriteString("\n")
}

func renderDetailRow(buf *strings.Builder, name string, st *simpleStats, pcts []int64) {
	n := st.count
	if n == 0 {
		buf.WriteString(name)
		buf.WriteString(" │")
		// min + avg + pcts + max = 3 + len(pcts) dashes
		for i := 0; i < 3+len(pcts); i++ {
			fmt.Fprintf(buf, " %8s", "-")
		}
		fmt.Fprintf(buf, " │ %9s\n", "0")
		return
	}
	buf.WriteString(name)
	buf.WriteString(" │")
	fmt.Fprintf(buf, " %s %s", formatLatencyPadded(st.min), formatLatencyPadded(st.Avg()))
	for _, p := range pcts {
		fmt.Fprintf(buf, " %s", formatLatencyPadded(p))
	}
	fmt.Fprintf(buf, " %s │ %9s\n", formatLatencyPadded(st.max), formatCount(int64(n)))
}

func (d *Display) renderSummary(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats, elapsed time.Duration, globalStats *simpleStats, globalPcts []int64) {
	entries := collectEntries(procStats, false)

	totalSecs := elapsed.Seconds()
	nPerCol := d.topN // rows per column
	if nPerCol <= 0 {
		nPerCol = (len(entries) + 1) / 2
	}
	totalShown := nPerCol * 2
	if totalShown > len(entries) {
		totalShown = len(entries)
	}

	slw := summaryLineWidth(len(d.quantiles))
	dualWidth := slw + 3 + slw

	// Column headers: LIFETIME │ avg [pcts...] max │ samples rate
	var hdr strings.Builder
	fmt.Fprintf(&hdr, "%-28s │", "LIFETIME")
	fmt.Fprintf(&hdr, " %8s", "avg")
	for _, q := range d.quantiles {
		fmt.Fprintf(&hdr, " %8s", quantileHeader(q))
	}
	fmt.Fprintf(&hdr, " %8s │ %9s %9s", "max", "samples", "rate")
	hdrStr := hdr.String()
	fmt.Fprintf(buf, "%s │ %s\n", hdrStr, hdrStr)
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
			leftStr = d.formatSummaryRow(leftSlice[i].label, leftSlice[i].ss.stats, sketchPercentiles(leftSlice[i].ss.sketch, d.quantiles), totalSecs)
		} else {
			leftStr = strings.Repeat(" ", slw)
		}

		if i < len(rightSlice) {
			rightStr = d.formatSummaryRow(rightSlice[i].label, rightSlice[i].ss.stats, sketchPercentiles(rightSlice[i].ss.sketch, d.quantiles), totalSecs)
		} else {
			rightStr = strings.Repeat(" ", slw)
		}

		fmt.Fprintf(buf, "%s │ %s\n", leftStr, rightStr)
	}

	// Build summary bar: LIFETIME(all) stats == title == legend ==
	title := fmt.Sprintf("Process × Syscall (top %d)", totalShown)
	legend := d.summaryBarLegend()

	globalRow := d.formatSummaryRow("LIFETIME(all)", globalStats, globalPcts, totalSecs)
	remaining := dualWidth - slw - 1
	if remaining < 0 {
		remaining = 0
	}
	fmt.Fprintf(buf, "%s %s\n", globalRow, buildSepLine(remaining, title, legend))
}

func (d *Display) formatSummaryRow(name string, st *simpleStats, pcts []int64, secs float64) string {
	var b strings.Builder
	n := st.count
	if n == 0 {
		fmt.Fprintf(&b, "%-28s │", name)
		// avg + pcts + max = 2 + len(pcts) dashes
		for i := 0; i < 2+len(pcts); i++ {
			fmt.Fprintf(&b, " %8s", "-")
		}
		fmt.Fprintf(&b, " │ %9s %9s", "0", "-")
		return b.String()
	}
	fmt.Fprintf(&b, "%-28s │", name)
	fmt.Fprintf(&b, " %s", formatLatencyPadded(st.Avg()))
	for _, p := range pcts {
		fmt.Fprintf(&b, " %s", formatLatencyPadded(p))
	}
	fmt.Fprintf(&b, " %s │ %9s %9s", formatLatencyPadded(st.max), formatCount(int64(n)), formatRate(n, secs))
	return b.String()
}

// renderProcPanel builds the right-side top-processes panel lines.
// maxRows limits the number of process rows (0 = unlimited).
func renderProcPanel(summaries []processSummary, matchedProcs map[string]bool, maxRows int) []string {
	var lines []string

	// Header
	lines = append(lines, padOrTrunc(" PROCESS         RATE   TOTAL", procPanelWidth))
	lines = append(lines, strings.Repeat("─", procPanelWidth))

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
		line := fmt.Sprintf(" %-15s %8s %7s", padOrTrunc(ps.name, 15), rateStr, formatCount(int64(ps.count)))
		lines = append(lines, padOrTrunc(line, procPanelWidth))
		n++
	}
	return lines
}
