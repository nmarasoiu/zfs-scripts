package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	colorRed     = "\033[0;31m"
	colorYellow  = "\033[1;33m"
	colorGreen   = "\033[0;32m"
	colorCyan    = "\033[0;36m"
	colorBold    = "\033[1m"
	colorDim     = "\033[2m"
	colorReset   = "\033[0m"
)

type TXG struct {
	TxgNum   uint64
	Birth    uint64
	State    string
	NDirty   uint64
	NRead    uint64
	NWritten uint64
	Reads    uint64
	Writes   uint64
	OTime    uint64
	QTime    uint64
	WTime    uint64
	STime    uint64
}

type SortColumn int

const (
	SortNone SortColumn = iota
	SortTxg
	SortDirty
	SortRead
	SortWritten
	SortOpen
	SortQueue
	SortWait
	SortSync
	SortMbps
)

type App struct {
	pools      []string
	interval   time.Duration
	txgCount   int
	sortCol    SortColumn
	sortRev    bool
	pageOffset int
	totalTxgs  int
	bootEpoch  int64
}

func main() {
	var (
		poolsStr string
		interval int
		txgCount int
		help     bool
	)

	flag.StringVar(&poolsStr, "pools", "hddpool ssdpool", "Space-separated pool names")
	flag.IntVar(&interval, "interval", 2, "Refresh interval in seconds")
	flag.IntVar(&txgCount, "count", 20, "Number of TXGs to display per pool")
	flag.BoolVar(&help, "h", false, "Show help")
	flag.BoolVar(&help, "help", false, "Show help")
	flag.Parse()

	// Also accept positional args for compatibility
	args := flag.Args()
	if len(args) >= 1 {
		poolsStr = args[0]
	}
	if len(args) >= 2 {
		if v, err := strconv.Atoi(args[1]); err == nil {
			interval = v
		}
	}
	if len(args) >= 3 {
		if v, err := strconv.Atoi(args[2]); err == nil {
			txgCount = v
		}
	}

	if help {
		printHelp()
		return
	}

	app := &App{
		pools:    strings.Fields(poolsStr),
		interval: time.Duration(interval) * time.Second,
		txgCount: txgCount,
		sortCol:  SortNone,
	}
	app.computeBootEpoch()
	app.run()
}

func printHelp() {
	fmt.Print(`Usage: top_txg [OPTIONS] [POOLS] [INTERVAL] [TXG_COUNT]

Monitor ZFS transaction groups (TXGs) with human-readable output.

Arguments:
  POOLS       Space-separated pool names (default: "hddpool ssdpool")
  INTERVAL    Refresh interval in seconds (default: 2)
  TXG_COUNT   Number of TXGs to display per pool (default: 20)

Options:
  -h, --help  Show this help message

Interactive Keys (lowercase=ascending, UPPERCASE=descending):
  t/T   Sort by TXG number (time)
  d/D   Sort by Dirty bytes
  r/R   Sort by Read bytes
  w/W   Sort by Written bytes
  o/O   Sort by Open time
  u/U   Sort by Queue time
  a/A   Sort by Wait time
  s/S   Sort by Sync time
  m/M   Sort by MB/s
  n     Reset to default (recent TXGs, no sorting)
  ↑/↓   Page up/down (only in sort modes)
  q     Quit

`)
}

func (app *App) computeBootEpoch() {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		app.bootEpoch = time.Now().Unix()
		return
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		app.bootEpoch = time.Now().Unix()
		return
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		app.bootEpoch = time.Now().Unix()
		return
	}
	app.bootEpoch = time.Now().Unix() - int64(uptime)
}

func (app *App) run() {
	// Set terminal to raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error setting raw mode: %v\n", err)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Hide cursor
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	// Clear screen
	fmt.Print("\033[2J")

	// Key input channel
	keyCh := make(chan byte, 10)
	go func() {
		buf := make([]byte, 3)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				keyCh <- buf[i]
			}
		}
	}()

	ticker := time.NewTicker(app.interval)
	defer ticker.Stop()

	app.render()

	for {
		select {
		case <-sigCh:
			return
		case <-ticker.C:
			app.render()
		case key := <-keyCh:
			if app.handleKey(key, keyCh) {
				return
			}
			app.render()
		}
	}
}

func (app *App) handleKey(key byte, keyCh chan byte) bool {
	switch key {
	case 'q', 'Q':
		return true
	case 'n', 'N':
		app.sortCol = SortNone
		app.pageOffset = 0
	case 't':
		app.sortCol = SortTxg
		app.sortRev = false
		app.pageOffset = 0
	case 'T':
		app.sortCol = SortTxg
		app.sortRev = true
		app.pageOffset = 0
	case 'd':
		app.sortCol = SortDirty
		app.sortRev = false
		app.pageOffset = 0
	case 'D':
		app.sortCol = SortDirty
		app.sortRev = true
		app.pageOffset = 0
	case 'r':
		app.sortCol = SortRead
		app.sortRev = false
		app.pageOffset = 0
	case 'R':
		app.sortCol = SortRead
		app.sortRev = true
		app.pageOffset = 0
	case 'w':
		app.sortCol = SortWritten
		app.sortRev = false
		app.pageOffset = 0
	case 'W':
		app.sortCol = SortWritten
		app.sortRev = true
		app.pageOffset = 0
	case 'o':
		app.sortCol = SortOpen
		app.sortRev = false
		app.pageOffset = 0
	case 'O':
		app.sortCol = SortOpen
		app.sortRev = true
		app.pageOffset = 0
	case 'u':
		app.sortCol = SortQueue
		app.sortRev = false
		app.pageOffset = 0
	case 'U':
		app.sortCol = SortQueue
		app.sortRev = true
		app.pageOffset = 0
	case 'a':
		app.sortCol = SortWait
		app.sortRev = false
		app.pageOffset = 0
	case 'A':
		app.sortCol = SortWait
		app.sortRev = true
		app.pageOffset = 0
	case 's':
		app.sortCol = SortSync
		app.sortRev = false
		app.pageOffset = 0
	case 'S':
		app.sortCol = SortSync
		app.sortRev = true
		app.pageOffset = 0
	case 'm':
		app.sortCol = SortMbps
		app.sortRev = false
		app.pageOffset = 0
	case 'M':
		app.sortCol = SortMbps
		app.sortRev = true
		app.pageOffset = 0
	case 0x1b: // Escape sequence
		select {
		case b := <-keyCh:
			if b == '[' {
				select {
				case arrow := <-keyCh:
					if app.sortCol != SortNone {
						switch arrow {
						case 'A': // Up
							app.pageOffset -= app.txgCount
							if app.pageOffset < 0 {
								app.pageOffset = 0
							}
						case 'B': // Down
							newOffset := app.pageOffset + app.txgCount
							if newOffset < app.totalTxgs {
								app.pageOffset = newOffset
							}
						}
					}
				case <-time.After(10 * time.Millisecond):
				}
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

func (app *App) render() {
	var sb strings.Builder

	// Move cursor home
	sb.WriteString("\033[H")

	// Title
	sortInfo := app.getSortInfo()
	sb.WriteString(fmt.Sprintf("%sZFS TXG Monitor%s %s(refresh: %v, %d TXGs/pool)%s",
		colorBold, colorReset, colorDim, app.interval, app.txgCount, colorReset))
	if app.sortCol != SortNone {
		dir := "▲"
		if app.sortRev {
			dir = "▼"
		}
		sb.WriteString(fmt.Sprintf("  %s%s %s%s", colorDim, sortInfo, dir, colorReset))
	}
	sb.WriteString("\n")

	// Keys
	sb.WriteString(fmt.Sprintf("%sKeys: [t/T]xg [d/D]irty [r/R]ead [w/W]ritten [o/O]pen q[u/U]eue w[a/A]it [s/S]ync [m/M]b/s  [n]one  [q]uit  [↑/↓]page%s\n",
		colorDim, colorReset))

	for _, pool := range app.pools {
		txgs, err := app.readTxgs(pool)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%sPool '%s' not found%s\n", colorRed, pool, colorReset))
			continue
		}

		app.totalTxgs = len(txgs)

		// Sort and paginate
		displayTxgs := app.sortAndPaginate(txgs)

		// Header
		app.writePoolHeader(&sb, pool)

		// TXG rows
		for _, txg := range displayTxgs {
			app.writeTxgRow(&sb, txg)
		}

		// Summary
		app.writeSummary(&sb, txgs)
		sb.WriteString("\n")
	}

	// Clear to end of screen
	sb.WriteString("\033[J")

	fmt.Print(sb.String())
}

func (app *App) getSortInfo() string {
	switch app.sortCol {
	case SortNone:
		return "recent"
	case SortTxg:
		return "TXG"
	case SortDirty:
		return "DIRTY"
	case SortRead:
		return "READ"
	case SortWritten:
		return "WRITTEN"
	case SortOpen:
		return "OPEN"
	case SortQueue:
		return "QUEUE"
	case SortWait:
		return "WAIT"
	case SortSync:
		return "SYNC"
	case SortMbps:
		return "MB/s"
	}
	return ""
}

func (app *App) readTxgs(pool string) ([]TXG, error) {
	path := fmt.Sprintf("/proc/spl/kstat/zfs/%s/txgs", pool)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var txgs []TXG
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "txg") {
			continue
		}
		txg := app.parseTxgLine(line)
		if txg != nil {
			txgs = append(txgs, *txg)
		}
	}
	return txgs, nil
}

func (app *App) parseTxgLine(line string) *TXG {
	fields := strings.Fields(line)
	if len(fields) < 12 {
		return nil
	}

	txg := &TXG{}
	txg.TxgNum, _ = strconv.ParseUint(fields[0], 10, 64)
	txg.Birth, _ = strconv.ParseUint(fields[1], 10, 64)
	txg.State = fields[2]
	txg.NDirty, _ = strconv.ParseUint(fields[3], 10, 64)
	txg.NRead, _ = strconv.ParseUint(fields[4], 10, 64)
	txg.NWritten, _ = strconv.ParseUint(fields[5], 10, 64)
	txg.Reads, _ = strconv.ParseUint(fields[6], 10, 64)
	txg.Writes, _ = strconv.ParseUint(fields[7], 10, 64)
	txg.OTime, _ = strconv.ParseUint(fields[8], 10, 64)
	txg.QTime, _ = strconv.ParseUint(fields[9], 10, 64)
	txg.WTime, _ = strconv.ParseUint(fields[10], 10, 64)
	txg.STime, _ = strconv.ParseUint(fields[11], 10, 64)

	return txg
}

func (app *App) sortAndPaginate(txgs []TXG) []TXG {
	if app.sortCol == SortNone {
		// Just return last N (most recent)
		start := len(txgs) - app.txgCount
		if start < 0 {
			start = 0
		}
		return txgs[start:]
	}

	// Sort
	sorted := make([]TXG, len(txgs))
	copy(sorted, txgs)

	sort.Slice(sorted, func(i, j int) bool {
		var less bool
		switch app.sortCol {
		case SortTxg:
			less = sorted[i].TxgNum < sorted[j].TxgNum
		case SortDirty:
			less = sorted[i].NDirty < sorted[j].NDirty
		case SortRead:
			less = sorted[i].NRead < sorted[j].NRead
		case SortWritten:
			less = sorted[i].NWritten < sorted[j].NWritten
		case SortOpen:
			less = sorted[i].OTime < sorted[j].OTime
		case SortQueue:
			less = sorted[i].QTime < sorted[j].QTime
		case SortWait:
			less = sorted[i].WTime < sorted[j].WTime
		case SortSync:
			less = sorted[i].STime < sorted[j].STime
		case SortMbps:
			mbpsI := float64(0)
			mbpsJ := float64(0)
			if sorted[i].STime > 0 {
				mbpsI = float64(sorted[i].NWritten) * 953.674 / float64(sorted[i].STime)
			}
			if sorted[j].STime > 0 {
				mbpsJ = float64(sorted[j].NWritten) * 953.674 / float64(sorted[j].STime)
			}
			less = mbpsI < mbpsJ
		}
		if app.sortRev {
			return !less
		}
		return less
	})

	// Paginate
	start := app.pageOffset
	end := start + app.txgCount
	if start >= len(sorted) {
		start = len(sorted) - app.txgCount
		if start < 0 {
			start = 0
		}
	}
	if end > len(sorted) {
		end = len(sorted)
	}
	return sorted[start:end]
}

func (app *App) writePoolHeader(sb *strings.Builder, pool string) {
	sortInfo := app.getSortInfo()
	sortIndicator := fmt.Sprintf("[%s]", sortInfo)
	if app.sortCol != SortNone {
		dir := "▲"
		if app.sortRev {
			dir = "▼"
		}
		sortIndicator = fmt.Sprintf("[sorted by %s %s] [%d+%d of %d]",
			sortInfo, dir, app.pageOffset, app.txgCount, app.totalTxgs)
	}

	sep := "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	sb.WriteString(fmt.Sprintf("%s%s%s  %s%s%s\n", colorBold, colorCyan, pool, colorDim, sortIndicator, colorReset))
	sb.WriteString(fmt.Sprintf("%s%s%s\n", colorBold, sep, colorReset))
	sb.WriteString(fmt.Sprintf("%s%-11s %-9s %-10s %-9s %-10s %-10s %-10s %-13s %-8s %-8s %-8s %-8s %-8s %s%s\n",
		colorBold, "DATE", "TIME", "TXG", "STATE", "DIRTY", "READ", "WRITTEN", "R/W OPS", "OPEN", "QUEUE", "WAIT", "SYNC", "MB/s", "DURATION", colorReset))
	sb.WriteString(fmt.Sprintf("%s%s%s\n", colorBold, sep, colorReset))
}

func (app *App) writeTxgRow(sb *strings.Builder, txg TXG) {
	birthDate, birthTime := app.birthToWallclock(txg.Birth)
	stateStr := app.stateLabel(txg.State)
	dirtyH := humanBytes(txg.NDirty)
	readH := humanBytes(txg.NRead)
	writtenH := humanBytes(txg.NWritten)
	otimeH := humanTimeNs(txg.OTime)
	qtimeH := humanTimeNs(txg.QTime)
	wtimeH := humanTimeNs(txg.WTime)
	stimeH := humanTimeNs(txg.STime)

	mbpsH := "-"
	if txg.STime > 0 && txg.NWritten > 0 {
		mbps := float64(txg.NWritten) * 953.674 / float64(txg.STime)
		mbpsH = fmt.Sprintf("%.1f", mbps)
	}

	durationStr := "-"
	if txg.State == "C" && birthTime != "" {
		completionHrtime := txg.Birth + txg.OTime + txg.QTime + txg.WTime + txg.STime
		_, endTime := app.birthToWallclock(completionHrtime)
		if endTime != "" {
			durationStr = fmt.Sprintf("%s -> %s", birthTime, endTime)
		}
	}

	color := colorDim
	if txg.State == "O" || txg.State == "S" || txg.State == "Q" {
		color = colorCyan
	}

	sb.WriteString(fmt.Sprintf("%s%-11s %-9s %-10d%s%s%s%-10s %-10s %-10s %6d/%-6d %-8s %-8s %-8s %-8s %-8s %s\n",
		color, birthDate, birthTime, txg.TxgNum, colorReset,
		stateStr, color,
		dirtyH, readH, writtenH, txg.Reads, txg.Writes,
		otimeH, qtimeH, wtimeH, stimeH, mbpsH, durationStr))
}

func (app *App) stateLabel(state string) string {
	switch state {
	case "O":
		return fmt.Sprintf("%sOPEN%s     ", colorGreen, colorReset)
	case "Q":
		return fmt.Sprintf("%sQUIESCE%s  ", colorYellow, colorReset)
	case "S":
		return fmt.Sprintf("%sSYNCING%s  ", colorRed, colorReset)
	case "C":
		return fmt.Sprintf("%sdone%s     ", colorDim, colorReset)
	default:
		return fmt.Sprintf("%s%-9s%s", colorDim, state, colorReset)
	}
}

func (app *App) birthToWallclock(birthHrtime uint64) (string, string) {
	if birthHrtime == 0 {
		return "", ""
	}
	birthSeconds := int64(birthHrtime / 1000000000)
	birthEpoch := app.bootEpoch + birthSeconds
	t := time.Unix(birthEpoch, 0)
	return t.Format("2006-01-02"), t.Format("15:04:05")
}

func (app *App) writeSummary(sb *strings.Builder, txgs []TXG) {
	// Get last 10 committed TXGs
	var committed []TXG
	for i := len(txgs) - 1; i >= 0 && len(committed) < 10; i-- {
		if txgs[i].State == "C" {
			committed = append(committed, txgs[i])
		}
	}

	if len(committed) == 0 {
		return
	}

	var sumStime, sumWritten, maxStime uint64
	var sumMbps float64
	var mbpsCount int

	for _, t := range committed {
		sumStime += t.STime
		sumWritten += t.NWritten
		if t.STime > maxStime {
			maxStime = t.STime
		}
		if t.STime > 0 {
			sumMbps += float64(t.NWritten) * 953.674 / float64(t.STime)
			mbpsCount++
		}
	}

	n := uint64(len(committed))
	avgStime := sumStime / n
	avgWritten := sumWritten / n
	avgMbps := float64(0)
	if mbpsCount > 0 {
		avgMbps = sumMbps / float64(mbpsCount)
	}

	sb.WriteString(fmt.Sprintf("%s  └─ Last %d avg: sync=%s, written=%s, max_sync=%s, avg_MB/s=%.1f%s\n",
		colorDim, len(committed), humanTimeNs(avgStime), humanBytes(avgWritten), humanTimeNs(maxStime), avgMbps, colorReset))
}

func humanBytes(bytes uint64) string {
	if bytes >= 1073741824 {
		return fmt.Sprintf("%.1fG", float64(bytes)/1073741824)
	} else if bytes >= 1048576 {
		return fmt.Sprintf("%.1fM", float64(bytes)/1048576)
	} else if bytes >= 1024 {
		return fmt.Sprintf("%.0fK", float64(bytes)/1024)
	}
	return fmt.Sprintf("%dB", bytes)
}

func humanTimeNs(ns uint64) string {
	if ns >= 60000000000 {
		return fmt.Sprintf("%.1fm", float64(ns)/60000000000)
	} else if ns >= 1000000000 {
		return fmt.Sprintf("%.1fs", float64(ns)/1000000000)
	}
	ms := (ns + 500000) / 1000000
	return fmt.Sprintf("%dms", ms)
}
