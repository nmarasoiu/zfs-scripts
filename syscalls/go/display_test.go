package main

import (
	"strings"
	"testing"
)

// --- collectEntries ---

func TestCollectEntries_SortedByCountDescending(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"tor": {
			0: statsWithCount(100), // read
			1: statsWithCount(50),  // write
		},
		"sshd": {
			0: statsWithCount(200), // read
		},
	}

	entries := collectEntries(procStats, false)
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[0].ss.stats.count != 200 {
		t.Errorf("first entry count = %d, want 200", entries[0].ss.stats.count)
	}
	if entries[1].ss.stats.count != 100 {
		t.Errorf("second entry count = %d, want 100", entries[1].ss.stats.count)
	}
	if entries[2].ss.stats.count != 50 {
		t.Errorf("third entry count = %d, want 50", entries[2].ss.stats.count)
	}
}

func TestCollectEntries_TiesBrokenByLabel(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"b": {0: statsWithCount(10)},
		"a": {0: statsWithCount(10)},
	}

	entries := collectEntries(procStats, false)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].label >= entries[1].label {
		t.Errorf("expected alphabetical tie-break: %q vs %q", entries[0].label, entries[1].label)
	}
}

func TestCollectEntries_SingleProcOmitsPrefix(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"tor": {0: statsWithCount(10)}, // syscall 0 = "read"
	}

	entries := collectEntries(procStats, true)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if strings.Contains(entries[0].label, "tor/") {
		t.Errorf("label should omit proc prefix in single-proc mode, got %q", entries[0].label)
	}
}

func TestCollectEntries_MultiProcIncludesPrefix(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"tor":  {0: statsWithCount(10)},
		"sshd": {0: statsWithCount(5)},
	}

	entries := collectEntries(procStats, false)
	for _, e := range entries {
		if !strings.Contains(e.label, "/") {
			t.Errorf("label should include proc prefix, got %q", e.label)
		}
	}
}

// --- filterStatsGeneral ---

func TestFilterStatsGeneral_MatchProcess(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"tor":  {0: statsWithCount(10), 1: statsWithCount(5)},
		"sshd": {0: statsWithCount(20)},
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
	procStats := map[string]map[uint32]*syscallStats{
		"tor":  {0: statsWithCount(10)}, // 0 = "read"
		"sshd": {1: statsWithCount(20)}, // 1 = "write"
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
	procStats := map[string]map[uint32]*syscallStats{
		"tor": {0: statsWithCount(10)},
	}

	filtered := filterStatsGeneral(procStats, "nonexistent")
	if len(filtered) != 0 {
		t.Errorf("filtered len = %d, want 0", len(filtered))
	}
}

func TestFilterStatsGeneral_CaseInsensitive(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"Tor": {0: statsWithCount(10)},
	}

	filtered := filterStatsGeneral(procStats, "tor")
	if len(filtered) != 1 {
		t.Errorf("filtered len = %d, want 1 (case-insensitive match)", len(filtered))
	}
}

// --- collectProcessSummaries ---

func TestCollectProcessSummaries_AggregatesAcrossSyscalls(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"tor": {
			0: statsWithCount(100),
			1: statsWithCount(50),
		},
	}

	summaries := collectProcessSummaries(procStats, 10.0)
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

func TestCollectProcessSummaries_SortedByCountDesc(t *testing.T) {
	procStats := map[string]map[uint32]*syscallStats{
		"low":  {0: statsWithCount(10)},
		"high": {0: statsWithCount(100)},
		"mid":  {0: statsWithCount(50)},
	}

	summaries := collectProcessSummaries(procStats, 1.0)
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
	procStats := map[string]map[uint32]*syscallStats{
		"p": {0: statsWithCount(10)},
	}

	summaries := collectProcessSummaries(procStats, 0)
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
	if !strings.Contains(legend, "[/]") || !strings.Contains(legend, "[q]") {
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
	d.renderFooter(&buf, 10*1e9, 5, 42, nil)
	s := buf.String()
	if !strings.Contains(s, "42") {
		t.Errorf("footer should contain drop count 42, got %q", s)
	}
	if !strings.Contains(s, "Processes: 5") {
		t.Errorf("footer should contain process count, got %q", s)
	}
}

func TestRenderFooter_WithRingStats(t *testing.T) {
	d := &Display{}
	var buf strings.Builder
	rs := &ringStats{
		capacityStats: capacityStats{avg: 4096, max: 8192, cap: 8 * 1024 * 1024},
		pending:       1024,
	}
	d.renderFooter(&buf, 10*1e9, 3, 0, rs)
	s := buf.String()
	if !strings.Contains(s, "Ring") {
		t.Errorf("footer should contain Ring info, got %q", s)
	}
}

// --- formatSummaryRow ---

func TestFormatSummaryRow_NonZero(t *testing.T) {
	ss := newSyscallStats()
	for i := int64(1); i <= 100; i++ {
		ss.Record(i)
	}
	row := formatSummaryRow("tor/read", ss.stats, sketchPercentiles(ss.sketch), 10.0)
	if !strings.Contains(row, "tor/read") {
		t.Errorf("row should contain name, got %q", row)
	}
	if !strings.Contains(row, "/s") {
		t.Errorf("row should contain rate, got %q", row)
	}
}

func TestFormatSummaryRow_Zero(t *testing.T) {
	ss := newSyscallStats()
	row := formatSummaryRow("empty", ss.stats, sketchPercentiles(ss.sketch), 10.0)
	if !strings.Contains(row, "-") {
		t.Errorf("zero row should contain dashes, got %q", row)
	}
}

// --- renderDetailRow ---

func TestRenderDetailRow_NonZero(t *testing.T) {
	ss := newSyscallStats()
	for i := int64(1); i <= 50; i++ {
		ss.Record(i)
	}
	var buf strings.Builder
	renderDetailRow(&buf, "tor/read        ", ss.stats, sketchPercentiles(ss.sketch))
	s := buf.String()
	if !strings.Contains(s, "tor/read") {
		t.Errorf("row should contain name, got %q", s)
	}
}

func TestRenderDetailRow_Zero(t *testing.T) {
	ss := newSyscallStats()
	var buf strings.Builder
	renderDetailRow(&buf, "empty           ", ss.stats, sketchPercentiles(ss.sketch))
	s := buf.String()
	if !strings.Contains(s, "-") {
		t.Errorf("zero row should contain dashes, got %q", s)
	}
}

// --- helper ---

func statsWithCount(n uint64) *syscallStats {
	ss := newSyscallStats()
	for i := uint64(0); i < n; i++ {
		ss.Record(int64(i + 1))
	}
	return ss
}
