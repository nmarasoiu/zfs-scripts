package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
)

// --- Display data transforms (turn State into renderable structures) ---

type processSummary struct {
	name  string
	count uint64
	rate  float64
}

type tableEntry struct {
	label   string
	sketch  *ddsketch.DDSketch
	sortVal float64
}

// entrySortVal extracts the sort value for a given column from a DDSketch.
func (d *Display) entrySortVal(sketch *ddsketch.DDSketch, elapsedSecs float64) float64 {
	count := sketch.GetCount()
	if count == 0 {
		return 0
	}
	switch d.sortColumn {
	case "avg":
		return sketch.GetSum() / count
	case "max":
		v, _ := sketch.GetMaxValue()
		return v
	case "samples", "count", "total":
		return count
	case "rate":
		if elapsedSecs > 0 {
			return count / elapsedSecs
		}
		return 0
	default:
		// percentile column: match against quantile headers
		for _, q := range d.quantiles {
			if d.sortColumn == quantileHeader(q) {
				v, _ := sketch.GetValueAtQuantile(q)
				return v
			}
		}
		return count
	}
}

// collectEntries builds a sorted list of table entries from per-process stats.
// When singleProc is true, the proc prefix is omitted from labels.
// Entries are sorted descending by d.sortColumn.
func (d *Display) collectEntries(procStats map[string]map[uint32]*ddsketch.DDSketch, singleProc bool, elapsedSecs float64) []tableEntry {

	var entries []tableEntry
	for proc, syscalls := range procStats {
		for id, sk := range syscalls {
			label := syscallName(id)
			if !singleProc {
				label = proc + "/" + label
			}
			sortVal := d.entrySortVal(sk, elapsedSecs)
			entries = append(entries, tableEntry{label, sk, sortVal})
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
func filterStatsGeneral(procStats map[string]map[uint32]*ddsketch.DDSketch, text string) map[string]map[uint32]*ddsketch.DDSketch {
	lower := strings.ToLower(text)
	filtered := make(map[string]map[uint32]*ddsketch.DDSketch)
	for proc, syscalls := range procStats {
		if strings.HasPrefix(strings.ToLower(proc), lower) {
			filtered[proc] = syscalls
			continue
		}
		matched := make(map[uint32]*ddsketch.DDSketch)
		for id, sk := range syscalls {
			if strings.HasPrefix(syscallName(id), lower) {
				matched[id] = sk
			}
		}
		if len(matched) > 0 {
			filtered[proc] = matched
		}
	}
	return filtered
}

// collectProcessSummaries aggregates per-process totals from persistent counters, sorted by d.sortColumn.
func (d *Display) collectProcessSummaries(procCounters map[string]*processCounter, elapsedSecs float64) []processSummary {
	sortByRate := d.sortColumn == "rate"
	summaries := make([]processSummary, 0, len(procCounters))
	for proc, pc := range procCounters {
		rate := float64(0)
		if elapsedSecs > 0 {
			rate = float64(pc.count) / elapsedSecs
		}
		summaries = append(summaries, processSummary{name: proc, count: pc.count, rate: rate})
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
	procPanelWidth      = 35
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
	sortColumn     string     // column to sort by (rate, samples, avg, p99, max, etc.)

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

// sketchPercentiles extracts d.quantiles from a DDSketch.
// Returns values in µs. Only called at display rate.
func (d *Display) sketchPercentiles(sketch *ddsketch.DDSketch) []int64 {
	if sketch == nil {
		return nil
	}
	result := make([]int64, len(d.quantiles))
	for i, q := range d.quantiles {
		v, _ := sketch.GetValueAtQuantile(q)
		result[i] = int64(v)
	}
	return result
}

// availableSortColumns returns the valid sort column names for the current view.
func (d *Display) availableSortColumns() []string {
	cols := []string{"avg"}
	for _, q := range d.quantiles {
		cols = append(cols, quantileHeader(q))
	}
	cols = append(cols, "max", "samples", "rate")
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

// rowWidth returns total row width: nameWidth + separator + data columns.
// Layout: %-nameWidth + " │" + avg [pcts...] max + " │" + samples + rate
func (d *Display) rowWidth(nameWidth int) int {
	// nameWidth + 2 ( │) + 9*(2+numPcts) (avg + pcts + max) + 3 ( │ ) + 9 (samples) + 1 ( ) + 9 (rate)
	return nameWidth + 2 + 9*(2+len(d.quantiles)) + 3 + 19
}

// writeHeader writes the column header line: "LIFETIME │ avg [pcts...] max │ samples rate"
// followed by a dash separator line.
func (d *Display) writeHeader(buf *strings.Builder, nameWidth int, lineWidth int) {
	nameFmt := fmt.Sprintf("%%-%ds", nameWidth)
	buf.WriteString(fmt.Sprintf(nameFmt, "LIFETIME"))
	buf.WriteString(" │")
	fmt.Fprintf(buf, " %s", d.sortIndicator("avg", 8))
	for _, q := range d.quantiles {
		fmt.Fprintf(buf, " %s", d.sortIndicator(quantileHeader(q), 8))
	}
	fmt.Fprintf(buf, " %s │ %s %s\n", d.sortIndicator("max", 8), d.sortIndicator("samples", 9), d.sortIndicator("rate", 9))
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")
}

// formatRow formats a single data row with the given name, sketch, and elapsed time.
// The name is padded/truncated to nameWidth. No trailing newline.
func (d *Display) formatRow(name string, nameWidth int, sketch *ddsketch.DDSketch, elapsedSecs float64) string {
	nameFmt := fmt.Sprintf("%%-%ds", nameWidth)
	paddedName := fmt.Sprintf(nameFmt, truncateProcLabel(name, nameWidth))

	var buf strings.Builder
	numPcts := len(d.quantiles)
	count := uint64(sketch.GetCount())
	if count == 0 {
		buf.WriteString(paddedName)
		buf.WriteString(" │")
		for i := 0; i < 2+numPcts; i++ {
			fmt.Fprintf(&buf, " %8s", "-")
		}
		fmt.Fprintf(&buf, " │ %9s %9s", "0", "-")
		return buf.String()
	}
	avg := int64(sketch.GetSum() / sketch.GetCount())
	maxVal, _ := sketch.GetMaxValue()

	buf.WriteString(paddedName)
	buf.WriteString(" │")
	fmt.Fprintf(&buf, " %s", formatLatencyPadded(avg))
	percentiles := d.sketchPercentiles(sketch)
	for _, pct := range percentiles {
		fmt.Fprintf(&buf, " %s", formatLatencyPadded(pct))
	}
	fmt.Fprintf(&buf, " %s │ %9s %9s", formatLatencyPadded(int64(maxVal)), formatCount(int64(count)), formatRate(count, elapsedSecs))
	return buf.String()
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

func (d *Display) render(state *State, frame frameMetrics) {
	var mainBuf strings.Builder
	var elapsed time.Duration
	var nProcs int
	var filterMatched map[string]bool // non-nil when filter is active

	state.Read(func(v StateView) {
		now := time.Now()
		elapsed = now.Sub(v.StartTime)

		const sketchBytesEach = 2048 // ~2KB per DDSketch: 0.7KB narrow, 4KB wide range (measured via protobuf + struct overhead)
		totalMB := float64(v.NSketches) * sketchBytesEach / 1024 / 1024

		cpuPct := 0.0
		if elapsed.Seconds() > 0 {
			cpuPct = frame.cpuTime.Seconds() / elapsed.Seconds() * 100
		}
		fmt.Fprintf(&mainBuf, "Syscall Latency Monitor - %s (uptime: %s, cpu: %s = %.1f%%) -- %d sketches × 2KB ≈ %.1fMB  churn:%s\n",
			now.Format("15:04:05"), formatDuration(elapsed), formatDuration(frame.cpuTime), cpuPct, v.NSketches, totalMB, formatCount(int64(v.SketchEvictions)))

		d.lastSummaries = d.collectProcessSummaries(v.ProcCounters, elapsed.Seconds())

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
			d.renderSummary(&mainBuf, viewStats, elapsed)
		}

		nProcs = len(v.ProcStats)
	})

	// Build footer
	var footerBuf strings.Builder
	d.renderFooter(&footerBuf, elapsed, nProcs, frame)

	// Compose main content with top-processes panel when terminal is wide enough.
	// Panel display is independent of interactive mode — --cols enables it in batch too.
	mainStr := mainBuf.String()
	footerStr := footerBuf.String()
	termWidth, _ := termSize()
	if d.colsOverride > 0 {
		termWidth = d.colsOverride
	}

	mainLines := strings.Split(strings.TrimRight(mainStr, "\n"), "\n")

	// Compute max display width of main content to decide if panel fits.
	maxContentWidth := 0
	for _, line := range mainLines {
		if w := displayWidth(line); w > maxContentWidth {
			maxContentWidth = w
		}
	}
	showProcPanel := termWidth >= maxContentWidth+procPanelOverhead

	var output strings.Builder

	if showProcPanel {
		procPanelMaxRows := d.topN + 4
		procPanelLines := renderProcPanel(d.lastSummaries, filterMatched, procPanelMaxRows)

		// mainColWidth = space allocated to main content (display columns).
		mainColWidth := termWidth - procPanelOverhead

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
			if ev.ch == '/' {
				// switch to filter mode, discard sort input
				d.sortText = ""
				d.mode = modeFilter
				d.filterText = ""
			} else if ev.ch == 's' && d.sortText == "" {
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

func (d *Display) renderFooter(buf *strings.Builder, elapsed time.Duration, nProcs int, frame frameMetrics) {
	total := frame.drops.total()
	dropRate := float64(0)
	if elapsed.Seconds() > 0 {
		dropRate = float64(total) / elapsed.Seconds()
	}
	dropDetail := fmt.Sprintf("ring:%s miss:%s",
		formatCount(int64(frame.drops.ringFull)),
		formatCount(int64(frame.drops.miss)))
	mapInfo := ""
	if frame.mapStats != nil {
		mapInfo = fmt.Sprintf(" | Map %s",
			frame.mapStats.formatUsage(formatCount))
	}
	ringInfo := ""
	if frame.ringStats != nil {
		ringInfo = fmt.Sprintf(" | Ring %s  cur: %6s  avg1:%-6.0f",
			frame.ringStats.formatUsage(formatBytes),
			formatBytes(int64(frame.ringStats.pending)),
			frame.ringStats.avg1)
	}
	fmt.Fprintf(buf, "Processes: %d | Drops: %s (%s/s) [%s]%s%s\n",
		nProcs, formatCount(int64(total)), formatCount(int64(dropRate)), dropDetail, mapInfo, ringInfo)

	if d.batchMode {
		buf.WriteString("\n")
	}
}

func (d *Display) renderTable(buf *strings.Builder, procStats map[string]map[uint32]*ddsketch.DDSketch, elapsedSecs float64) {
	entries := d.collectEntries(procStats, len(procStats) == 1, elapsedSecs)

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
	lineWidth := d.rowWidth(labelWidth)
	sectionHeader(buf, fmt.Sprintf("%s (%d)", title, shown), lineWidth)

	d.writeHeader(buf, labelWidth, lineWidth)

	// Data rows
	for i := 0; i < shown; i++ {
		e := entries[i]
		buf.WriteString(d.formatRow(e.label, labelWidth, e.sketch, elapsedSecs))
		buf.WriteByte('\n')
	}

	buf.WriteString("\n")
}

func (d *Display) renderSummary(buf *strings.Builder, procStats map[string]map[uint32]*ddsketch.DDSketch, elapsed time.Duration) {
	entries := d.collectEntries(procStats, false, elapsed.Seconds())

	totalSecs := elapsed.Seconds()
	rowsPerCol := d.topN // rows per column
	if rowsPerCol <= 0 {
		rowsPerCol = (len(entries) + 1) / 2
	}
	totalShown := rowsPerCol * 2
	if totalShown > len(entries) {
		totalShown = len(entries)
	}

	const nameWidth = 28
	colWidth := d.rowWidth(nameWidth)
	dualWidth := colWidth + 3 + colWidth

	// Dual column headers
	var header strings.Builder
	d.writeHeader(&header, nameWidth, colWidth)
	// writeHeader produces "header\n---\n"; extract the header line (before first \n)
	headerLines := strings.SplitN(header.String(), "\n", 3)
	headerStr := headerLines[0]
	fmt.Fprintf(buf, "%s │ %s\n", headerStr, headerStr)
	buf.WriteString(strings.Repeat("-", dualWidth))
	buf.WriteString("\n")

	leftEnd := rowsPerCol
	if leftEnd > len(entries) {
		leftEnd = len(entries)
	}
	rightStart := rowsPerCol
	rightEnd := rowsPerCol * 2
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
			leftStr = d.formatRow(leftSlice[i].label, nameWidth, leftSlice[i].sketch, totalSecs)
		} else {
			leftStr = strings.Repeat(" ", colWidth)
		}

		if i < len(rightSlice) {
			rightStr = d.formatRow(rightSlice[i].label, nameWidth, rightSlice[i].sketch, totalSecs)
		} else {
			rightStr = strings.Repeat(" ", colWidth)
		}

		fmt.Fprintf(buf, "%s │ %s\n", leftStr, rightStr)
	}

	// Summary bar with title and interactive legend
	title := fmt.Sprintf("Process × Syscall (top %d)", totalShown)
	legend := d.summaryBarLegend()
	fmt.Fprintf(buf, "%s\n", buildSepLine(dualWidth, title, legend))
}

// renderProcPanel builds the right-side top-processes panel lines.
// maxRows limits the number of process rows (0 = unlimited).
func renderProcPanel(summaries []processSummary, matchedProcs map[string]bool, maxRows int) []string {
	var lines []string

	// Header
	lines = append(lines, padOrTrunc(fmt.Sprintf(" %-15s %8s %7s", "PROCESS", "RATE", "TOTAL"), procPanelWidth))
	lines = append(lines, strings.Repeat("─", procPanelWidth))

	n := 0
	for _, summary := range summaries {
		if matchedProcs != nil && !matchedProcs[summary.name] {
			continue
		}
		if maxRows > 0 && n >= maxRows {
			break
		}
		rateStr := "-"
		if summary.rate >= 1 {
			rateStr = formatCount(int64(summary.rate)) + "/s"
		} else if summary.rate > 0 {
			rateStr = fmt.Sprintf("%.1f/s", summary.rate)
		}
		line := fmt.Sprintf(" %-15s %8s %7s", padOrTrunc(summary.name, 15), rateStr, formatCount(int64(summary.count)))
		lines = append(lines, padOrTrunc(line, procPanelWidth))
		n++
	}
	return lines
}
