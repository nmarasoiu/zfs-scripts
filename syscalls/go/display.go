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
	label   string
	ss      *syscallStats
	sortVal float64
}

// entrySortVal extracts the sort value for a given column from a syscallStats entry.
func entrySortVal(ss *syscallStats, col string, elapsedSecs float64, quantiles []float64) float64 {
	switch col {
	case "min":
		return float64(ss.stats.min)
	case "avg":
		return float64(ss.stats.Avg())
	case "max":
		return float64(ss.stats.max)
	case "samples", "count", "total":
		return float64(ss.stats.count)
	case "rate":
		if elapsedSecs > 0 {
			return float64(ss.stats.count) / elapsedSecs
		}
		return 0
	default:
		// percentile column: match against quantile headers
		for _, q := range quantiles {
			if col == quantileHeader(q) {
				v, _ := ss.sketch.GetValueAtQuantile(q)
				return v
			}
		}
		return float64(ss.stats.count)
	}
}

// collectEntries builds a sorted list of table entries from per-process stats.
// When singleProc is true, the proc prefix is omitted from labels.
// Entries are sorted descending by the given sortColumn.
func collectEntries(procStats map[string]map[uint32]*syscallStats, singleProc bool, sortColumn string, elapsedSecs float64, quantiles []float64) []tableEntry {

	var entries []tableEntry
	for proc, fm := range procStats {
		for id, ss := range fm {
			label := syscallName(id)
			if !singleProc {
				label = proc + "/" + label
			}
			sv := entrySortVal(ss, sortColumn, elapsedSecs, quantiles)
			entries = append(entries, tableEntry{label, ss, sv})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].sortVal != entries[j].sortVal {
			return entries[i].sortVal > entries[j].sortVal
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
// When sortByRate is true, summaries are sorted by rate/sec; otherwise by total count.
func collectProcessSummaries(procStats map[string]map[uint32]*syscallStats, elapsedSecs float64, sortColumn string) []processSummary {
	sortByRate := sortColumn == "rate"
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
	if sortByRate {
		sort.Slice(summaries, func(i, j int) bool {
			if summaries[i].rate != summaries[j].rate {
				return summaries[i].rate > summaries[j].rate
			}
			return summaries[i].name < summaries[j].name
		})
	} else {
		sort.Slice(summaries, func(i, j int) bool {
			if summaries[i].count != summaries[j].count {
				return summaries[i].count > summaries[j].count
			}
			return summaries[i].name < summaries[j].name
		})
	}
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
	modeSort
)

// Display handles rendering
type Display struct {
	batchMode      bool
	focusProcesses []string   // ordered list of focus process names
	topN           int
	colsOverride   int        // --cols override; 0 = auto-detect
	quantiles      []float64  // configured percentile quantiles (0.0–1.0)
	sortColumn     string     // column to sort by (rate, samples, avg, p99, max, min, etc.)

	// Interactive state (all owned by display goroutine — no sync needed)
	interactive   bool
	mode          interactiveMode
	filterText    string
	sortText      string // typed text in modeSort
	lastSummaries []processSummary
}

// quantileHeader formats a quantile as a column header (e.g. 0.50 → "p50", 0.999 → "p99.9").
func quantileHeader(q float64) string {
	return fmt.Sprintf("p%g", q*100)
}

// availableSortColumns returns the valid sort column names for the current view.
// Table view (focusProcesses set) has min but no rate; summary view has rate but no min.
func (d *Display) availableSortColumns() []string {
	isTable := len(d.focusProcesses) > 0
	var cols []string
	if isTable {
		cols = append(cols, "min")
	}
	cols = append(cols, "avg")
	for _, q := range d.quantiles {
		cols = append(cols, quantileHeader(q))
	}
	cols = append(cols, "max", "samples")
	if !isTable {
		cols = append(cols, "rate")
	}
	return cols
}

// isValidSortColumn checks if the given column name is valid for the current view.
func (d *Display) isValidSortColumn(col string) bool {
	for _, c := range d.availableSortColumns() {
		if c == col {
			return true
		}
	}
	return false
}

// sortIndicator returns the column header with ▼ appended if it's the active sort column.
// The result is right-justified to the given width.
func (d *Display) sortIndicator(col string, width int) string {
	if col == d.sortColumn || (col == "samples" && (d.sortColumn == "count" || d.sortColumn == "total")) {
		s := col + "▼"
		// ▼ is 3 bytes but 1 display column, so pad to width-1 display columns
		return fmt.Sprintf("%*s", width-1, s)
	}
	return fmt.Sprintf("%*s", width, col)
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
		return "[/] filter  [s] sort  [q] quit"
	case modeFilter:
		return fmt.Sprintf("Filter: %s_  [/] cancel  [Bksp] back", d.filterText)
	case modeSort:
		return fmt.Sprintf("Sort by: %s_  [columns: %s]  [s] cancel", d.sortText, strings.Join(d.availableSortColumns(), " "))
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

		d.lastSummaries = collectProcessSummaries(v.ProcStats, elapsed.Seconds(), d.sortColumn)

		viewStats := v.ProcStats
		if d.mode == modeFilter && d.filterText != "" {
			viewStats = filterStatsGeneral(viewStats, d.filterText)
			filterMatched = make(map[string]bool, len(viewStats))
			for proc := range viewStats {
				filterMatched[proc] = true
			}
		}

		if len(d.focusProcesses) > 0 {
			d.renderTable(&mainBuf, viewStats, elapsed.Seconds())
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
			output.WriteString("  [/] filter  [s] sort  [q] quit\n")
		case modeFilter:
			fmt.Fprintf(&output, "  Filter prefix (proc/syscall): %s_  [/] cancel  [Bksp] back\n", d.filterText)
		case modeSort:
			fmt.Fprintf(&output, "  Sort by: %s_  [columns: %s]  [s] cancel\n", d.sortText, strings.Join(d.availableSortColumns(), " "))
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
			case 's':
				d.mode = modeSort
				d.sortText = ""
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
	case modeSort:
		switch ev.kind {
		case keyChar:
			if ev.ch == 's' && d.sortText == "" {
				// 's' again on empty → cancel
				d.mode = modeNormal
			} else {
				d.sortText += string(ev.ch)
				d.tryAutoSelectSort()
			}
		case keyBackspace:
			if len(d.sortText) > 0 {
				d.sortText = d.sortText[:len(d.sortText)-1]
			} else {
				d.mode = modeNormal
			}
		}
	}
	return false
}

// tryAutoSelectSort checks if the current sortText uniquely matches one column.
// If so, applies the sort and returns to normal mode.
func (d *Display) tryAutoSelectSort() {
	cols := d.availableSortColumns()
	lower := strings.ToLower(d.sortText)
	var matches []string
	for _, c := range cols {
		if strings.HasPrefix(strings.ToLower(c), lower) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 1 {
		d.sortColumn = matches[0]
		d.sortText = ""
		d.mode = modeNormal
	}
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

func (d *Display) renderTable(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats, elapsedSecs float64) {
	entries := collectEntries(procStats, len(procStats) == 1, d.sortColumn, elapsedSecs, d.quantiles)

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
	fmt.Fprintf(buf, " %s %s", d.sortIndicator("min", 8), d.sortIndicator("avg", 8))
	for _, q := range d.quantiles {
		fmt.Fprintf(buf, " %s", d.sortIndicator(quantileHeader(q), 8))
	}
	fmt.Fprintf(buf, " %s │ %s\n", d.sortIndicator("max", 8), d.sortIndicator("samples", 9))
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
	entries := collectEntries(procStats, false, d.sortColumn, elapsed.Seconds(), d.quantiles)

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
	fmt.Fprintf(&hdr, " %s", d.sortIndicator("avg", 8))
	for _, q := range d.quantiles {
		fmt.Fprintf(&hdr, " %s", d.sortIndicator(quantileHeader(q), 8))
	}
	fmt.Fprintf(&hdr, " %s │ %s %s", d.sortIndicator("max", 8), d.sortIndicator("samples", 9), d.sortIndicator("rate", 9))
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
