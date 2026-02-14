package main

import (
	"strings"
	"testing"

	"github.com/DataDog/sketches-go/ddsketch"
)

var defaultQuantiles = []float64{0.50, 0.90, 0.99}

// testDisplay creates a minimal Display for testing sort/collect functions.
func testDisplay(sortColumn string) *Display {
	return &Display{sortColumn: sortColumn, quantiles: defaultQuantiles}
}

// --- collectEntries ---

func TestCollectEntries_SortedByRateDescending(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor": {
			0: sketchWithCount(100), // read
			1: sketchWithCount(50),  // write
		},
		"sshd": {
			0: sketchWithCount(200), // read
		},
	}

	// sortByRate=true with elapsed=10s: rates are 20/s, 10/s, 5/s
	entries := testDisplay("rate").collectEntries(procStats, false, 10.0)
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if uint64(entries[0].sketch.GetCount()) != 200 {
		t.Errorf("first entry count = %d, want 200", uint64(entries[0].sketch.GetCount()))
	}
	if uint64(entries[1].sketch.GetCount()) != 100 {
		t.Errorf("second entry count = %d, want 100", uint64(entries[1].sketch.GetCount()))
	}
	if uint64(entries[2].sketch.GetCount()) != 50 {
		t.Errorf("third entry count = %d, want 50", uint64(entries[2].sketch.GetCount()))
	}
}

func TestCollectEntries_SortedByCountDescending(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor": {
			0: sketchWithCount(100), // read
			1: sketchWithCount(50),  // write
		},
		"sshd": {
			0: sketchWithCount(200), // read
		},
	}

	// sortByRate=false: sorted by total count
	entries := testDisplay("samples").collectEntries(procStats, false, 10.0)
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if uint64(entries[0].sketch.GetCount()) != 200 {
		t.Errorf("first entry count = %d, want 200", uint64(entries[0].sketch.GetCount()))
	}
	if uint64(entries[1].sketch.GetCount()) != 100 {
		t.Errorf("second entry count = %d, want 100", uint64(entries[1].sketch.GetCount()))
	}
	if uint64(entries[2].sketch.GetCount()) != 50 {
		t.Errorf("third entry count = %d, want 50", uint64(entries[2].sketch.GetCount()))
	}
}

func TestCollectEntries_TiesBrokenByLabel(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"b": {0: sketchWithCount(10)},
		"a": {0: sketchWithCount(10)},
	}

	entries := testDisplay("rate").collectEntries(procStats, false, 10.0)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].label >= entries[1].label {
		t.Errorf("expected alphabetical tie-break: %q vs %q", entries[0].label, entries[1].label)
	}
}

func TestCollectEntries_SingleProcOmitsPrefix(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor": {0: sketchWithCount(10)}, // syscall 0 = "read"
	}

	entries := testDisplay("rate").collectEntries(procStats, true, 10.0)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].label, "tor/") {
		t.Errorf("label should omit proc prefix in single-proc mode, got %q", entries[0].label)
	}
}

func TestCollectEntries_MultiProcIncludesPrefix(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor":  {0: sketchWithCount(10)},
		"sshd": {0: sketchWithCount(5)},
	}

	entries := testDisplay("rate").collectEntries(procStats, false, 10.0)
	for _, e := range entries {
		if !strings.Contains(e.label, "/") {
			t.Errorf("label should include proc prefix, got %q", e.label)
		}
	}
}

// --- filterStatsGeneral ---

func TestFilterStatsGeneral_MatchProcess(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor":  {0: sketchWithCount(10), 1: sketchWithCount(5)},
		"sshd": {0: sketchWithCount(20)},
	}

	filtered := filterStatsGeneral(procStats, "tor")
	if len(filtered) != 1 {
		t.Fatalf("filtered len = %d, want 1", len(filtered))
	}
	if filtered["tor"] == nil {
		t.Error("expected tor in filtered results")
	}
	// All tor syscalls should be included when process name matches
	if len(filtered["tor"]) != 2 {
		t.Errorf("tor syscalls = %d, want 2", len(filtered["tor"]))
	}
}

func TestFilterStatsGeneral_MatchSyscall(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor":  {0: sketchWithCount(10)}, // 0 = "read"
		"sshd": {1: sketchWithCount(20)}, // 1 = "write"
	}

	filtered := filterStatsGeneral(procStats, "read")
	if len(filtered) != 1 {
		t.Fatalf("filtered len = %d, want 1", len(filtered))
	}
	if filtered["tor"] == nil {
		t.Error("expected tor in filtered results (has 'read' syscall)")
	}
}

func TestFilterStatsGeneral_NoMatch(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"tor": {0: sketchWithCount(10)},
	}

	filtered := filterStatsGeneral(procStats, "nonexistent")
	if len(filtered) != 0 {
		t.Errorf("filtered len = %d, want 0", len(filtered))
	}
}

func TestFilterStatsGeneral_CaseInsensitive(t *testing.T) {
	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"Tor": {0: sketchWithCount(10)},
	}

	filtered := filterStatsGeneral(procStats, "tor")
	if len(filtered) != 1 {
		t.Errorf("filtered len = %d, want 1 (case-insensitive match)", len(filtered))
	}
}

// --- collectProcessSummaries ---

func TestCollectProcessSummaries_AggregatesAcrossSyscalls(t *testing.T) {
	procCounters := map[string]*processCounter{
		"tor": {count: 150},
	}

	summaries := testDisplay("rate").collectProcessSummaries(procCounters, 10.0)
	if len(summaries) != 1 {
		t.Fatalf("len = %d, want 1", len(summaries))
	}
	if summaries[0].count != 150 {
		t.Errorf("total count = %d, want 150", summaries[0].count)
	}
	if summaries[0].rate != 15.0 {
		t.Errorf("rate = %f, want 15.0", summaries[0].rate)
	}
}

func TestCollectProcessSummaries_SortedByRateDesc(t *testing.T) {
	procCounters := map[string]*processCounter{
		"low":  {count: 10},
		"high": {count: 100},
		"mid":  {count: 50},
	}

	summaries := testDisplay("rate").collectProcessSummaries(procCounters, 1.0)
	if len(summaries) != 3 {
		t.Fatalf("len = %d, want 3", len(summaries))
	}
	if summaries[0].name != "high" {
		t.Errorf("first = %q, want high", summaries[0].name)
	}
	if summaries[1].name != "mid" {
		t.Errorf("second = %q, want mid", summaries[1].name)
	}
	if summaries[2].name != "low" {
		t.Errorf("third = %q, want low", summaries[2].name)
	}
}

func TestCollectProcessSummaries_ZeroElapsed(t *testing.T) {
	procCounters := map[string]*processCounter{
		"p": {count: 10},
	}

	summaries := testDisplay("rate").collectProcessSummaries(procCounters, 0)
	if summaries[0].rate != 0 {
		t.Errorf("rate = %f, want 0 when elapsed=0", summaries[0].rate)
	}
}

// --- handleKey ---

func TestHandleKey_QuitInNormalMode(t *testing.T) {
	d := &Display{interactive: true}
	quit := d.handleKey(keyEvent{kind: keyChar, ch: 'q'})
	if !quit {
		t.Error("expected quit=true for 'q' in normal mode")
	}
}

func TestHandleKey_SlashEntersFilterMode(t *testing.T) {
	d := &Display{interactive: true}
	quit := d.handleKey(keyEvent{kind: keyChar, ch: '/'})
	if quit {
		t.Error("expected quit=false for '/' in normal mode")
	}
	if d.mode != modeFilter {
		t.Errorf("mode = %d, want modeFilter", d.mode)
	}
}

func TestHandleKey_FilterTyping(t *testing.T) {
	d := &Display{interactive: true, mode: modeFilter}
	d.handleKey(keyEvent{kind: keyChar, ch: 't'})
	d.handleKey(keyEvent{kind: keyChar, ch: 'o'})
	d.handleKey(keyEvent{kind: keyChar, ch: 'r'})

	if d.filterText != "tor" {
		t.Errorf("filterText = %q, want %q", d.filterText, "tor")
	}
}

func TestHandleKey_FilterBackspace(t *testing.T) {
	d := &Display{interactive: true, mode: modeFilter, filterText: "tor"}
	d.handleKey(keyEvent{kind: keyBackspace})
	if d.filterText != "to" {
		t.Errorf("filterText = %q, want %q", d.filterText, "to")
	}
}

func TestHandleKey_FilterBackspaceEmptyExitsFilterMode(t *testing.T) {
	d := &Display{interactive: true, mode: modeFilter, filterText: ""}
	d.handleKey(keyEvent{kind: keyBackspace})
	if d.mode != modeNormal {
		t.Errorf("mode = %d, want modeNormal after backspace on empty filter", d.mode)
	}
}

func TestHandleKey_SlashCancelsFilter(t *testing.T) {
	d := &Display{interactive: true, mode: modeFilter, filterText: "tor"}
	d.handleKey(keyEvent{kind: keyChar, ch: '/'})
	if d.mode != modeNormal {
		t.Errorf("mode = %d, want modeNormal", d.mode)
	}
	if d.filterText != "" {
		t.Errorf("filterText = %q, want empty", d.filterText)
	}
}

func TestHandleKey_NonQuitCharInNormalMode(t *testing.T) {
	d := &Display{interactive: true}
	quit := d.handleKey(keyEvent{kind: keyChar, ch: 'x'})
	if quit {
		t.Error("expected quit=false for 'x' in normal mode")
	}
	if d.mode != modeNormal {
		t.Errorf("mode should stay modeNormal")
	}
}

// --- buildSepLine ---

func TestBuildSepLine_WithLegend(t *testing.T) {
	line := buildSepLine(50, "Title", "[q] quit")
	if !strings.HasPrefix(line, "== Title ") {
		t.Errorf("missing prefix, got %q", line)
	}
	if !strings.HasSuffix(line, " [q] quit ==") {
		t.Errorf("missing suffix, got %q", line)
	}
	if len(line) != 50 {
		t.Errorf("len = %d, want 50", len(line))
	}
}

func TestBuildSepLine_WithoutLegend(t *testing.T) {
	line := buildSepLine(30, "Title", "")
	if !strings.HasPrefix(line, "== Title ") {
		t.Errorf("missing prefix, got %q", line)
	}
	// Without legend, suffix is empty so line is all '=' fill
	if len(line) != 30 {
		t.Errorf("len = %d, want 30", len(line))
	}
}

// --- renderProcPanel ---

func TestRenderProcPanel_HeaderAndRows(t *testing.T) {
	summaries := []processSummary{
		{name: "tor", count: 1000, rate: 100},
		{name: "sshd", count: 500, rate: 50},
	}
	lines := renderProcPanel(summaries, nil, 0)
	if len(lines) < 4 { // header + separator + 2 data rows
		t.Fatalf("expected at least 4 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "PROCESS") {
		t.Errorf("first line should be header, got %q", lines[0])
	}
}

func TestRenderProcPanel_MaxRows(t *testing.T) {
	summaries := []processSummary{
		{name: "a", count: 100, rate: 10},
		{name: "b", count: 50, rate: 5},
		{name: "c", count: 25, rate: 2.5},
	}
	lines := renderProcPanel(summaries, nil, 2)
	dataLines := lines[2:] // skip header + separator
	if len(dataLines) != 2 {
		t.Errorf("data lines = %d, want 2 (maxRows=2)", len(dataLines))
	}
}

func TestRenderProcPanel_FilteredByMatchedProcs(t *testing.T) {
	summaries := []processSummary{
		{name: "tor", count: 100, rate: 10},
		{name: "sshd", count: 50, rate: 5},
	}
	matched := map[string]bool{"tor": true}
	lines := renderProcPanel(summaries, matched, 0)
	dataLines := lines[2:]
	if len(dataLines) != 1 {
		t.Errorf("data lines = %d, want 1 (only tor matched)", len(dataLines))
	}
	if !strings.Contains(dataLines[0], "tor") {
		t.Errorf("expected tor row, got %q", dataLines[0])
	}
}

// --- sectionHeader ---

func TestSectionHeader(t *testing.T) {
	var buf strings.Builder
	sectionHeader(&buf, "test", 30)
	s := buf.String()
	if !strings.HasPrefix(s, "── test ") {
		t.Errorf("missing prefix, got %q", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Errorf("should end with newline")
	}
}

// --- summaryBarLegend ---

func TestSummaryBarLegend_NonInteractive(t *testing.T) {
	d := &Display{interactive: false}
	if d.summaryBarLegend() != "" {
		t.Error("non-interactive should return empty legend")
	}
}

func TestSummaryBarLegend_NormalMode(t *testing.T) {
	d := &Display{interactive: true, mode: modeNormal}
	legend := d.summaryBarLegend()
	if !strings.Contains(legend, "[/]") || !strings.Contains(legend, "[q]") || !strings.Contains(legend, "[s]") {
		t.Errorf("normal mode legend = %q, missing keys", legend)
	}
}

func TestSummaryBarLegend_FilterMode(t *testing.T) {
	d := &Display{interactive: true, mode: modeFilter, filterText: "tor"}
	legend := d.summaryBarLegend()
	if !strings.Contains(legend, "tor") {
		t.Errorf("filter mode legend = %q, missing filter text", legend)
	}
}

// --- renderFooter ---

func TestRenderFooter_ContainsDropInfo(t *testing.T) {
	d := &Display{}
	var buf strings.Builder
	d.renderFooter(&buf, 10*1e9, 5, frameMetrics{drops: frameDrops{ringFull: 42}})
	s := buf.String()
	if !strings.Contains(s, "42") {
		t.Errorf("footer should contain drop count 42, got %q", s)
	}
	if !strings.Contains(s, "Processes: 5") {
		t.Errorf("footer should contain process count, got %q", s)
	}
}

func TestRenderFooter_WithMapStats(t *testing.T) {
	d := &Display{}
	var buf strings.Builder
	d.renderFooter(&buf, 10*1e9, 5, frameMetrics{
		mapStats: &mapStats{
			capacityStats: capacityStats{avg: 1234, max: 2000, cap: 65536},
			cur:           1234,
		},
	})
	s := buf.String()
	if !strings.Contains(s, "Map") {
		t.Errorf("footer should contain Map info, got %q", s)
	}
	if !strings.Contains(s, "1.2K") {
		t.Errorf("footer should contain formatted map avg count, got %q", s)
	}
	if !strings.Contains(s, "65.5K") {
		t.Errorf("footer should contain formatted map cap, got %q", s)
	}
	if !strings.Contains(s, "max:") {
		t.Errorf("footer should contain max label, got %q", s)
	}
}

func TestRenderFooter_SplitDropReasons(t *testing.T) {
	d := &Display{}
	var buf strings.Builder
	d.renderFooter(&buf, 10*1e9, 5, frameMetrics{
		drops: frameDrops{ringFull: 10, miss: 110},
	})
	s := buf.String()
	if !strings.Contains(s, "miss:") {
		t.Errorf("footer should contain miss label, got %q", s)
	}
	if !strings.Contains(s, "120") { // total = 10+110
		t.Errorf("footer should contain total 120, got %q", s)
	}
}

func TestRenderFooter_WithRingStats(t *testing.T) {
	d := &Display{}
	var buf strings.Builder
	rs := &ringStats{
		capacityStats: capacityStats{avg: 4096, max: 8192, cap: 8 * 1024 * 1024},
		pending:       1024,
	}
	d.renderFooter(&buf, 10*1e9, 3, frameMetrics{ringStats: rs})
	s := buf.String()
	if !strings.Contains(s, "Ring") {
		t.Errorf("footer should contain Ring info, got %q", s)
	}
}

// --- formatSummaryRow ---

var testQuantiles = []float64{0.25, 0.50, 0.75, 0.90, 0.99, 0.999}

func TestFormatSummaryRow_NonZero(t *testing.T) {
	sk := newTestSketch(0.25)
	for i := int64(1); i <= 100; i++ {
		sk.Add(float64(i))
	}
	d := &Display{quantiles: testQuantiles}
	row := d.formatSummaryRow("tor/read", sk, 10.0)
	if !strings.Contains(row, "tor/read") {
		t.Errorf("row should contain name, got %q", row)
	}
	if !strings.Contains(row, "/s") {
		t.Errorf("row should contain rate, got %q", row)
	}
}

func TestFormatSummaryRow_Zero(t *testing.T) {
	sk := newTestSketch(0.25)
	d := &Display{quantiles: testQuantiles}
	row := d.formatSummaryRow("empty", sk, 10.0)
	if !strings.Contains(row, "-") {
		t.Errorf("zero row should contain dashes, got %q", row)
	}
}

// --- renderDetailRow ---

func TestRenderDetailRow_NonZero(t *testing.T) {
	sk := newTestSketch(0.25)
	for i := int64(1); i <= 50; i++ {
		sk.Add(float64(i))
	}
	d := &Display{quantiles: testQuantiles}
	var buf strings.Builder
	d.renderDetailRow(&buf, "tor/read        ", sk, 10.0)
	s := buf.String()
	if !strings.Contains(s, "tor/read") {
		t.Errorf("row should contain name, got %q", s)
	}
}

func TestRenderDetailRow_Zero(t *testing.T) {
	sk := newTestSketch(0.25)
	d := &Display{quantiles: testQuantiles}
	var buf strings.Builder
	d.renderDetailRow(&buf, "empty           ", sk, 10.0)
	s := buf.String()
	if !strings.Contains(s, "-") {
		t.Errorf("zero row should contain dashes, got %q", s)
	}
}

// --- availableSortColumns ---

func TestAvailableSortColumns_SummaryView(t *testing.T) {
	d := &Display{quantiles: []float64{0.50, 0.99}}
	cols := d.availableSortColumns()
	// Summary view: avg, p50, p99, max, samples, rate
	expected := []string{"avg", "p50", "p99", "max", "samples", "rate"}

	if len(cols) != len(expected) {
		t.Fatalf("cols = %v, want %v", cols, expected)
	}
	for i, c := range cols {
		if c != expected[i] {
			t.Errorf("cols[%d] = %q, want %q", i, c, expected[i])
		}
	}
}

func TestAvailableSortColumns_TableView(t *testing.T) {
	d := &Display{focusProcesses: []string{"tor"}, quantiles: []float64{0.50, 0.99}}
	cols := d.availableSortColumns()
	// Table view: avg, p50, p99, max, samples, rate
	expected := []string{"avg", "p50", "p99", "max", "samples", "rate"}
	if len(cols) != len(expected) {
		t.Fatalf("cols = %v, want %v", cols, expected)
	}
	for i, c := range cols {
		if c != expected[i] {
			t.Errorf("cols[%d] = %q, want %q", i, c, expected[i])
		}
	}
}

// --- entrySortVal ---

func TestEntrySortVal_Rate(t *testing.T) {
	sk := sketchWithCount(100)
	val := testDisplay("rate").entrySortVal(sk, 10.0)
	if val != 10.0 {
		t.Errorf("rate sortVal = %f, want 10.0", val)
	}
}

func TestEntrySortVal_Samples(t *testing.T) {
	sk := sketchWithCount(100)
	val := testDisplay("samples").entrySortVal(sk, 10.0)
	if val != 100.0 {
		t.Errorf("samples sortVal = %f, want 100.0", val)
	}
}

func TestEntrySortVal_Avg(t *testing.T) {
	sk := sketchWithCount(100)
	expected := sk.GetSum() / sk.GetCount()
	val := testDisplay("avg").entrySortVal(sk, 10.0)
	if val != expected {
		t.Errorf("avg sortVal = %f, want %f", val, expected)
	}
}

func TestEntrySortVal_Max(t *testing.T) {
	sk := sketchWithCount(100)
	expected, _ := sk.GetMaxValue()
	maxVal := testDisplay("max").entrySortVal(sk, 10.0)
	if maxVal != expected {
		t.Errorf("max sortVal = %f, want %f", maxVal, expected)
	}
}

func TestEntrySortVal_Percentile(t *testing.T) {
	sk := sketchWithCount(100)
	val := testDisplay("p99").entrySortVal(sk, 10.0)
	if val <= 0 {
		t.Errorf("p99 sortVal = %f, want > 0", val)
	}
}

// --- sort by percentile column ---

func TestCollectEntries_SortedByPercentile(t *testing.T) {
	// Create two entries with different latency distributions
	skLow := newTestSketch(0.25)
	for i := int64(1); i <= 100; i++ {
		skLow.Add(float64(i)) // latencies 1-100
	}
	skHigh := newTestSketch(0.25)
	for i := int64(50); i <= 200; i++ {
		skHigh.Add(float64(i)) // latencies 50-200, higher p99
	}

	procStats := map[string]map[uint32]*ddsketch.DDSketch{
		"a": {0: skLow},
		"b": {0: skHigh},
	}

	entries := testDisplay("p99").collectEntries(procStats, false, 10.0)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	// b/read should be first (higher p99)
	if !strings.HasPrefix(entries[0].label, "b/") {
		t.Errorf("first entry = %q, want b/ prefix (higher p99)", entries[0].label)
	}
}

// --- sort mode key handling ---

func TestHandleKey_SEntersSortMode(t *testing.T) {
	d := &Display{interactive: true, quantiles: defaultQuantiles}
	quit := d.handleKey(keyEvent{kind: keyChar, ch: 's'})
	if quit {
		t.Error("expected quit=false for 's' in normal mode")
	}
	if d.mode != modeSort {
		t.Errorf("mode = %d, want modeSort", d.mode)
	}
}

func TestHandleKey_SortCancelWithS(t *testing.T) {
	d := &Display{interactive: true, mode: modeSort, sortText: "", quantiles: defaultQuantiles, sortColumn: "rate"}
	d.handleKey(keyEvent{kind: keyChar, ch: 's'})
	if d.mode != modeNormal {
		t.Errorf("mode = %d, want modeNormal (cancel sort)", d.mode)
	}
}

func TestHandleKey_SortBackspaceEmpty(t *testing.T) {
	d := &Display{interactive: true, mode: modeSort, sortText: "", quantiles: defaultQuantiles}
	d.handleKey(keyEvent{kind: keyBackspace})
	if d.mode != modeNormal {
		t.Errorf("mode = %d, want modeNormal after backspace on empty sort", d.mode)
	}
}

func TestHandleKey_SortAutoSelect(t *testing.T) {
	d := &Display{interactive: true, mode: modeSort, sortText: "", quantiles: []float64{0.50, 0.99}, sortColumn: "rate"}
	// Type "m" → uniquely matches "max", auto-select
	d.handleKey(keyEvent{kind: keyChar, ch: 'm'})
	if d.mode != modeNormal {
		t.Errorf("mode = %d, want modeNormal (auto-selected)", d.mode)
	}
	if d.sortColumn != "max" {
		t.Errorf("sortColumn = %q, want max", d.sortColumn)
	}
}

func TestHandleKey_SortPartialNoAutoSelect(t *testing.T) {
	d := &Display{interactive: true, mode: modeSort, sortText: "", quantiles: []float64{0.50, 0.99}, sortColumn: "rate"}
	// Type "p" → matches p50 and p99, should not auto-select
	d.handleKey(keyEvent{kind: keyChar, ch: 'p'})
	if d.mode != modeSort {
		t.Errorf("mode = %d, want modeSort (ambiguous prefix)", d.mode)
	}
	if d.sortText != "p" {
		t.Errorf("sortText = %q, want p", d.sortText)
	}
	// Now type "5" → matches only p50
	d.handleKey(keyEvent{kind: keyChar, ch: '5'})
	if d.mode != modeNormal {
		t.Errorf("mode = %d, want modeNormal (auto-selected p50)", d.mode)
	}
	if d.sortColumn != "p50" {
		t.Errorf("sortColumn = %q, want p50", d.sortColumn)
	}
}

// --- sortIndicator ---

func TestSortIndicator_ActiveColumn(t *testing.T) {
	d := &Display{sortColumn: "p99"}
	s := d.sortIndicator("p99", 8)
	if !strings.Contains(s, "p99▼") {
		t.Errorf("sortIndicator = %q, want to contain p99▼", s)
	}
}

func TestSortIndicator_InactiveColumn(t *testing.T) {
	d := &Display{sortColumn: "rate"}
	s := d.sortIndicator("p99", 8)
	if strings.Contains(s, "▼") {
		t.Errorf("sortIndicator = %q, should not contain ▼", s)
	}
}

// --- helpers ---

// newTestSketch creates a DDSketch for testing.
func newTestSketch(alpha float64) *ddsketch.DDSketch {
	sk, _ := ddsketch.NewDefaultDDSketch(alpha)
	return sk
}

// sketchWithCount creates a DDSketch with n samples (values 1..n).
func sketchWithCount(n uint64) *ddsketch.DDSketch {
	sk := newTestSketch(0.25)
	for i := uint64(0); i < n; i++ {
		sk.Add(float64(i + 1))
	}
	return sk
}
