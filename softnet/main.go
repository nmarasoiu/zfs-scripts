// softnet: network stack pressure monitor
//
// Reads /proc/net/softnet_stat (per-CPU softirq packet processing),
// /proc/net/snmp (TCP/UDP counters), and /proc/net/netstat (TcpExt)
// to show a live view of network pressure — drops, squeezes, retransmits,
// and listen queue overflows.
//
// Designed for high-connection storage/network servers (Tor, Storj, torrents)
// where packet-level stalls and retransmits are the silent killers.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	version = "dev"
	commit  = ""
)

var (
	interval    = flag.Duration("i", 2*time.Second, "sampling interval")
	batch       = flag.Bool("batch", false, "batch mode: print once per interval, no screen clearing")
	showHelp    = flag.Bool("help", false, "show detailed help")
	showVersion = flag.Bool("version", false, "print version and exit")
)

func pread(fd int, buf []byte) int {
	n, _ := syscall.Pread(fd, buf, 0)
	return n
}

func mustOpen(path string) int {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
		os.Exit(1)
	}
	return fd
}

// ── data types ──────────────────────────────────────────────────

type cpuSoftnet struct {
	processed uint64
	dropped   uint64
	squeeze   uint64
}

type tcpCounters struct {
	activeOpens  uint64
	passiveOpens uint64
	currEstab    uint64
	inSegs       uint64
	outSegs      uint64
	retransSegs  uint64
	inErrs       uint64
}

type udpCounters struct {
	inDatagrams  uint64
	outDatagrams uint64
	inErrors     uint64
	rcvbufErrors uint64
	sndbufErrors uint64
}

type tcpExtCounters struct {
	listenOverflows uint64
	listenDrops     uint64
	timeouts        uint64
	lossProbes      uint64
	backlogDrop     uint64
}

type snap struct {
	ts   time.Time
	cpus []cpuSoftnet
	tcp  tcpCounters
	udp  udpCounters
	ext  tcpExtCounters
}

// ── parsing ─────────────────────────────────────────────────────

func readSoftnet(fd int, buf []byte) []cpuSoftnet {
	n := pread(fd, buf)
	if n <= 0 {
		return nil
	}
	var cpus []cpuSoftnet
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		cpus = append(cpus, cpuSoftnet{
			processed: parseHex(fields[0]),
			dropped:   parseHex(fields[1]),
			squeeze:   parseHex(fields[2]),
		})
	}
	return cpus
}

func parseHex(s string) uint64 {
	v, _ := strconv.ParseUint(s, 16, 64)
	return v
}

// parseSnmpTable parses /proc/net/snmp or /proc/net/netstat.
// These files have pairs of lines: header then values, both with same prefix.
// Returns map[prefix]map[column_name]value.
func parseSnmpTable(fd int, buf []byte) map[string]map[string]uint64 {
	n := pread(fd, buf)
	if n <= 0 {
		return nil
	}
	result := make(map[string]map[string]uint64)
	lines := strings.Split(string(buf[:n]), "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		headers := strings.Fields(lines[i])
		values := strings.Fields(lines[i+1])
		if len(headers) < 2 || len(values) < 2 {
			continue
		}

		prefix := strings.TrimSuffix(headers[0], ":")
		if result[prefix] == nil {
			result[prefix] = make(map[string]uint64)
		}
		for j := 1; j < len(headers) && j < len(values); j++ {
			v, _ := strconv.ParseUint(values[j], 10, 64)
			result[prefix][headers[j]] = v
		}
	}
	return result
}

func readSnap(softnetFd, snmpFd, netstatFd int, buf []byte) snap {
	s := snap{ts: time.Now(), cpus: readSoftnet(softnetFd, buf)}

	snmp := parseSnmpTable(snmpFd, buf)
	if tcp, ok := snmp["Tcp"]; ok {
		s.tcp = tcpCounters{
			activeOpens:  tcp["ActiveOpens"],
			passiveOpens: tcp["PassiveOpens"],
			currEstab:    tcp["CurrEstab"],
			inSegs:       tcp["InSegs"],
			outSegs:      tcp["OutSegs"],
			retransSegs:  tcp["RetransSegs"],
			inErrs:       tcp["InErrs"],
		}
	}
	if udp, ok := snmp["Udp"]; ok {
		s.udp = udpCounters{
			inDatagrams:  udp["InDatagrams"],
			outDatagrams: udp["OutDatagrams"],
			inErrors:     udp["InErrors"],
			rcvbufErrors: udp["RcvbufErrors"],
			sndbufErrors: udp["SndbufErrors"],
		}
	}

	netstat := parseSnmpTable(netstatFd, buf)
	if ext, ok := netstat["TcpExt"]; ok {
		s.ext = tcpExtCounters{
			listenOverflows: ext["ListenOverflows"],
			listenDrops:     ext["ListenDrops"],
			timeouts:        ext["TCPTimeouts"],
			lossProbes:      ext["TCPLossProbes"],
			backlogDrop:     ext["TCPBacklogDrop"],
		}
	}
	return s
}

// ── formatting ──────────────────────────────────────────────────

func fmtCount(v uint64) string {
	switch {
	case v >= 1_000_000_000:
		return fmt.Sprintf("%.1fG", float64(v)/1e9)
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1e6)
	case v >= 10_000:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func fmtRate(delta uint64, secs float64) string {
	r := float64(delta) / secs
	switch {
	case r >= 1_000_000:
		return fmt.Sprintf("%.1fM/s", r/1e6)
	case r >= 1_000:
		return fmt.Sprintf("%.1fk/s", r/1e3)
	case r >= 1:
		return fmt.Sprintf("%.1f/s", r)
	case r > 0:
		return fmt.Sprintf("%.2f/s", r)
	default:
		return "0"
	}
}

func fmtPct(num, denom uint64) string {
	if denom == 0 {
		return ""
	}
	pct := float64(num) * 100.0 / float64(denom)
	if pct < 0.001 {
		return ""
	}
	if pct < 0.01 {
		return fmt.Sprintf("(%.4f%%)", pct)
	}
	if pct < 1 {
		return fmt.Sprintf("(%.3f%%)", pct)
	}
	return fmt.Sprintf("(%.1f%%)", pct)
}

// ── display ─────────────────────────────────────────────────────

func render(prev, cur snap, start time.Time) {
	dt := cur.ts.Sub(prev.ts).Seconds()
	if dt <= 0 {
		dt = 1
	}
	elapsed := cur.ts.Sub(start).Truncate(time.Second)

	if !*batch {
		fmt.Print("\033[2J\033[H")
	}

	fmt.Printf("softnet – network stack pressure (%v interval, up %v)\n", *interval, elapsed)
	fmt.Println("═══════════════════════════════════════════════════════════════════════")

	// ── Softirq per-CPU ──
	fmt.Printf("\n%-6s %10s %10s %10s %10s  │ %9s %9s\n",
		"", "pkt/s", "drop/s", "squeeze/s", "processed", "drops", "squeezes")
	fmt.Println("───────────────────────────────────────────────────────┼─────────────────────────────")

	var totalProc, totalDrop, totalSq uint64
	var prevTotalProc, prevTotalDrop, prevTotalSq uint64
	for i, c := range cur.cpus {
		totalProc += c.processed
		totalDrop += c.dropped
		totalSq += c.squeeze
		if i < len(prev.cpus) {
			prevTotalProc += prev.cpus[i].processed
			prevTotalDrop += prev.cpus[i].dropped
			prevTotalSq += prev.cpus[i].squeeze

			dp := c.processed - prev.cpus[i].processed
			dd := c.dropped - prev.cpus[i].dropped
			ds := c.squeeze - prev.cpus[i].squeeze
			fmt.Printf("  cpu%-2d %9s %10s %10s %10s  │ %9s %9s\n",
				i,
				fmtRate(dp, dt),
				fmtRate(dd, dt),
				fmtRate(ds, dt),
				fmtCount(c.processed),
				fmtCount(c.dropped),
				fmtCount(c.squeeze))
		}
	}
	fmt.Println("───────────────────────────────────────────────────────┼─────────────────────────────")
	fmt.Printf("  %-5s %9s %10s %10s %10s  │ %9s %9s\n",
		"ALL",
		fmtRate(totalProc-prevTotalProc, dt),
		fmtRate(totalDrop-prevTotalDrop, dt),
		fmtRate(totalSq-prevTotalSq, dt),
		fmtCount(totalProc),
		fmtCount(totalDrop),
		fmtCount(totalSq))

	// ── TCP ──
	fmt.Printf("\nTCP  (%s established)\n", fmtCount(cur.tcp.currEstab))
	fmt.Println("─────────────────────────────────────────────────────────────────")
	printRow("Retransmits", cur.tcp.retransSegs, prev.tcp.retransSegs, dt,
		fmtPct(cur.tcp.retransSegs-prev.tcp.retransSegs, cur.tcp.outSegs-prev.tcp.outSegs))
	printRow("Timeouts", cur.ext.timeouts, prev.ext.timeouts, dt, "")
	printRow("Listen drops", cur.ext.listenDrops, prev.ext.listenDrops, dt, "")
	printRow("Listen overflow", cur.ext.listenOverflows, prev.ext.listenOverflows, dt, "")
	printRow("Loss probes", cur.ext.lossProbes, prev.ext.lossProbes, dt, "")
	printRow("Backlog drops", cur.ext.backlogDrop, prev.ext.backlogDrop, dt, "")
	printRow("In errors", cur.tcp.inErrs, prev.tcp.inErrs, dt, "")

	// Connection rate
	activeD := cur.tcp.activeOpens - prev.tcp.activeOpens
	passiveD := cur.tcp.passiveOpens - prev.tcp.passiveOpens
	fmt.Printf("  %-19s %10s active  %10s passive\n", "Opens",
		fmtRate(activeD, dt), fmtRate(passiveD, dt))

	// ── UDP ──
	fmt.Println("\nUDP")
	fmt.Println("─────────────────────────────────────────────────────────────────")
	printRow("Errors", cur.udp.inErrors, prev.udp.inErrors, dt, "")
	printRow("Rcvbuf overflow", cur.udp.rcvbufErrors, prev.udp.rcvbufErrors, dt, "")
	printRow("Sndbuf overflow", cur.udp.sndbufErrors, prev.udp.sndbufErrors, dt, "")

	inD := cur.udp.inDatagrams - prev.udp.inDatagrams
	outD := cur.udp.outDatagrams - prev.udp.outDatagrams
	fmt.Printf("  %-19s %10s in      %10s out\n", "Throughput",
		fmtRate(inD, dt), fmtRate(outD, dt))
}

func printRow(label string, cur, prev uint64, dt float64, extra string) {
	d := cur - prev
	rate := fmtRate(d, dt)
	total := fmtCount(cur)
	if extra != "" {
		fmt.Printf("  %-19s %10s %12s total  %s\n", label, rate, total, extra)
	} else {
		fmt.Printf("  %-19s %10s %12s total\n", label, rate, total)
	}
}

// ── help ────────────────────────────────────────────────────────

func printHelp() {
	fmt.Print(`softnet – network stack pressure monitor

Shows live rates of network pressure indicators from the kernel.
Built for storage/network servers (Tor, Storj, torrents) where packet-level
stalls and retransmits silently degrade throughput and peer connections.

SOFTIRQ PER-CPU — /proc/net/softnet_stat
  pkt/s        Packets processed per second by this CPU's softirq.
  drop/s       Packets dropped because the CPU's backlog queue was full.
               → Fix: increase net.core.netdev_budget or net.core.netdev_max_backlog
  squeeze/s    Times the softirq ran out of its time/packet budget before
               finishing all queued work — remaining packets wait for the next cycle.
               → Non-zero = your NIC is delivering faster than the CPU can drain.
               → Fix: increase net.core.netdev_budget (default 300)
                       or net.core.netdev_budget_usecs (default 2000)
               → Also check: is one CPU handling all interrupts? (check /proc/interrupts)

TCP
  Retransmits  Segments re-sent (lost or unacknowledged). Rate as % of outSegs
               shows congestion severity. For Storj/torrents, high retransmits
               wastes upload bandwidth you could be earning from.
  Timeouts     Connections that hit RTO — peer is unreachable or too slow.
               High = peers see your node as flaky.
  Listen drops Incoming connections dropped because the accept queue was full.
               → Fix: increase net.core.somaxconn or per-app backlog
               → Critical for Tor relays and Storj nodes accepting peer connections.
  Loss probes  Tail Loss Probes (TLP) sent to detect lost final segments.
               Normal at moderate rates; spikes indicate lossy paths.
  Backlog drops Packets dropped from the per-socket backlog.
  In errors    Incoming segments with checksum errors or other problems.

UDP
  Errors       Incoming datagrams with errors (checksum failures).
  Rcvbuf overflow  Datagrams dropped because the socket receive buffer was full.
               → Fix: increase net.core.rmem_max and app SO_RCVBUF
               → Matters for BitTorrent DHT (UDP) and any UDP trackers.
  Sndbuf overflow  Datagrams dropped because the socket send buffer was full.

USAGE
  softnet                     Live dashboard, 2s interval
  softnet -i 5s               5-second sampling interval
  softnet -batch              One snapshot per interval (for logging/piping)
  softnet -batch -i 10s       Log every 10s

TUNING CHEATSHEET
  sysctl net.core.netdev_budget         # default 300, try 600
  sysctl net.core.netdev_budget_usecs   # default 2000, try 4000
  sysctl net.core.netdev_max_backlog    # default 1000, try 5000
  sysctl net.core.somaxconn             # default 4096, try 8192
  sysctl net.core.rmem_max              # default 212992, try 2097152
  cat /proc/interrupts | grep -i eth    # check IRQ balance across CPUs
`)
}

// ── main ────────────────────────────────────────────────────────

func main() {
	flag.Usage = func() { printHelp() }
	flag.Parse()
	if *showVersion {
		if commit != "" {
			fmt.Printf("softnet %s (%s)\n", version, commit)
		} else {
			fmt.Printf("softnet %s\n", version)
		}
		return
	}
	if *showHelp {
		printHelp()
		return
	}

	softnetFd := mustOpen("/proc/net/softnet_stat")
	defer syscall.Close(softnetFd)
	snmpFd := mustOpen("/proc/net/snmp")
	defer syscall.Close(snmpFd)
	netstatFd := mustOpen("/proc/net/netstat")
	defer syscall.Close(netstatFd)
	buf := make([]byte, 64*1024)

	start := time.Now()
	prev := readSnap(softnetFd, snmpFd, netstatFd, buf)
	time.Sleep(*interval)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	cur := readSnap(softnetFd, snmpFd, netstatFd, buf)
	render(prev, cur, start)
	prev = cur

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cur = readSnap(softnetFd, snmpFd, netstatFd, buf)
			render(prev, cur, start)
			prev = cur
		case <-sigCh:
			fmt.Println()
			return
		}
	}
}
