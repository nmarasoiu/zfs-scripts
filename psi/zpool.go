package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type zpDev struct {
	depth int
	name  string
	state string
	read  string
	write string
	cksum string
	slow  string
	notes string
}

type zpPool struct {
	name    string
	state   string
	scan    []string
	devices []zpDev
}

func shortenDev(name string) string {
	// usb-Seagate_Expansion_HDD_00000000SERIAL-0:0 -> Seagate:SERIAL
	if i := strings.Index(name, "Seagate_Expansion_HDD_"); i >= 0 {
		rest := name[i+len("Seagate_Expansion_HDD_"):]
		rest = strings.TrimLeft(rest, "0")
		if j := strings.Index(rest, "-"); j > 0 {
			rest = rest[:j]
		}
		return "Seagate:" + rest
	}
	// nvme-WD_BLACK_SN770_2TB_SERIAL-partN -> SN770:SERIAL:pN
	if strings.HasPrefix(name, "nvme-") {
		s := name[5:]
		part := ""
		if j := strings.LastIndex(s, "-part"); j >= 0 {
			part = ":p" + s[j+5:]
			s = s[:j]
		}
		if k := strings.Index(s, "SN"); k >= 0 {
			fields := strings.SplitN(s[k:], "_", 3)
			model := fields[0]
			serial := ""
			if len(fields) >= 3 {
				serial = fields[len(fields)-1]
				if len(serial) > 6 {
					serial = serial[:6]
				}
			}
			if serial != "" {
				return model + ":" + serial + part
			}
			return model + part
		}
		if len(s) > 20 {
			s = "…" + s[len(s)-15:]
		}
		return s + part
	}
	// wwn-0xHEX-partN -> wwn:LAST5:pN
	if strings.HasPrefix(name, "wwn-") {
		s := strings.TrimPrefix(name[4:], "0x")
		part := ""
		if j := strings.LastIndex(s, "-part"); j >= 0 {
			part = ":p" + s[j+5:]
			s = s[:j]
		}
		if len(s) > 5 {
			s = s[len(s)-5:]
		}
		return "wwn:" + s + part
	}
	if len(name) > 28 {
		return "…" + name[len(name)-27:]
	}
	return name
}

func condenseScan(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	full := strings.Join(lines, " ")

	if strings.Contains(full, "in progress") {
		scanType := "resilver"
		if strings.Contains(full, "scrub") {
			scanType = "scrub"
		}
		pct := ""
		if i := strings.Index(full, "% done"); i >= 0 {
			j := strings.LastIndexAny(full[:i], " ,") + 1
			pct = full[j:i] + "%"
		}
		issued := ""
		if i := strings.Index(full, " issued"); i >= 0 {
			chunk := full[:i]
			if j := strings.LastIndex(chunk, ","); j >= 0 {
				chunk = chunk[j+1:]
			}
			issued = strings.TrimSpace(chunk)
		}
		result := scanType
		if pct != "" {
			result += " " + pct
		}
		if issued != "" {
			result += " (" + issued + ")"
		}
		if strings.Contains(full, "no estimated") {
			result += ", no est."
		}
		return result
	}

	if strings.Contains(full, "repaired") {
		scanType := "scrub"
		if strings.Contains(full, "resilver") {
			scanType = "resilver"
		}
		dur := ""
		if i := strings.Index(full, " in "); i >= 0 {
			for _, w := range strings.Fields(full[i+4:]) {
				if strings.Count(w, ":") == 2 {
					dur = w
					break
				}
			}
		}
		errs := ""
		if i := strings.Index(full, " errors"); i >= 0 {
			j := strings.LastIndex(full[:i], " ") + 1
			errs = full[j:i] + " err"
		}
		result := scanType + " done"
		if dur != "" {
			result += " " + dur
		}
		if errs != "" {
			result += ", " + errs
		}
		return result
	}

	s := lines[0]
	if len(s) > 50 {
		s = s[:50] + "…"
	}
	return s
}

func parseZpoolStatus() ([]zpPool, error) {
	out, err := exec.Command("zpool", "status", "-sv").CombinedOutput()
	if err != nil {
		return nil, err
	}

	var pools []zpPool
	var cur *zpPool
	inConfig, inScan, pastHeader := false, false, false

	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "pool:"):
			pools = append(pools, zpPool{name: strings.TrimSpace(trimmed[5:])})
			cur = &pools[len(pools)-1]
			inConfig, inScan, pastHeader = false, false, false

		case cur == nil:
			continue

		case strings.HasPrefix(trimmed, "state:"):
			cur.state = strings.TrimSpace(trimmed[6:])
			inScan = false

		case strings.HasPrefix(trimmed, "scan:"):
			cur.scan = append(cur.scan, strings.TrimSpace(trimmed[5:]))
			inScan = true
			inConfig = false

		case strings.HasPrefix(trimmed, "config:"):
			inConfig = true
			inScan = false
			pastHeader = false

		case strings.HasPrefix(trimmed, "errors:") ||
			strings.HasPrefix(trimmed, "status:") ||
			strings.HasPrefix(trimmed, "action:") ||
			strings.HasPrefix(trimmed, "see:"):
			inConfig = false
			inScan = false

		case inScan && trimmed != "":
			cur.scan = append(cur.scan, trimmed)

		case inConfig:
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "NAME") {
				pastHeader = true
				continue
			}
			if !pastHeader {
				continue
			}

			// depth from spaces after leading tab
			spaces := 0
			pastTab := false
			for _, c := range line {
				if c == '\t' {
					pastTab = true
					spaces = 0
				} else if c == ' ' && pastTab {
					spaces++
				} else {
					break
				}
			}
			depth := spaces / 2

			fields := strings.Fields(trimmed)
			if len(fields) == 0 {
				continue
			}

			// vdev type labels
			switch fields[0] {
			case "special", "logs", "cache", "spare", "spares", "dedup":
				if len(fields) == 1 {
					cur.devices = append(cur.devices, zpDev{depth: depth, name: fields[0]})
					continue
				}
			}

			dev := zpDev{depth: depth, name: fields[0]}
			if len(fields) >= 2 {
				dev.state = fields[1]
			}
			if len(fields) >= 3 {
				dev.read = fields[2]
			}
			if len(fields) >= 4 {
				dev.write = fields[3]
			}
			if len(fields) >= 5 {
				dev.cksum = fields[4]
			}
			if len(fields) >= 6 {
				dev.slow = fields[5]
			}
			if len(fields) > 6 {
				dev.notes = strings.Join(fields[6:], " ")
			}
			cur.devices = append(cur.devices, dev)
		}
	}

	return pools, nil
}

// poolStateFd holds a pre-opened fd to /proc/spl/kstat/zfs/<pool>/state
type poolStateFd struct {
	name string
	fd   int
}

// discoverPools finds ZFS pools by scanning kstat for dirs with a state file.
func discoverPools() []poolStateFd {
	entries, err := os.ReadDir("/proc/spl/kstat/zfs")
	if err != nil {
		return nil
	}
	var fds []poolStateFd
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := "/proc/spl/kstat/zfs/" + e.Name() + "/state"
		fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
		if err != nil {
			continue
		}
		fds = append(fds, poolStateFd{name: e.Name(), fd: fd})
	}
	return fds
}

// refreshZpoolCache runs zpool status if the cache is stale.
func refreshZpoolCache(pools *[]zpPool, lastRefresh *time.Time, interval time.Duration) {
	if time.Since(*lastRefresh) < interval {
		return
	}
	parsed, err := parseZpoolStatus()
	if err != nil {
		return
	}
	*pools = parsed
	*lastRefresh = time.Now()
}

// updatePoolStates reads live pool state from kstat via pread (no subprocess).
func updatePoolStates(poolFds []poolStateFd, buf []byte, pools []zpPool) {
	for _, pf := range poolFds {
		n := pread(pf.fd, buf)
		if n <= 0 {
			continue
		}
		state := strings.TrimSpace(string(buf[:n]))
		for i := range pools {
			if pools[i].name == pf.name {
				pools[i].state = state
			}
		}
	}
}

func printZpoolStatus(w io.Writer, pools []zpPool) {
	for _, pool := range pools {
		// header
		stateStr := fmt.Sprintf("%-8s", pool.state)
		if pool.state != "ONLINE" {
			stateStr = ansiRed + ansiBold + stateStr + ansiReset
		}
		scan := condenseScan(pool.scan)

		fmt.Fprintf(w, "%-6s │ %-10s │ %s", "POOL", pool.name, stateStr)
		if scan != "" {
			fmt.Fprintf(w, " │ %s", scan)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "───────┼──────────────────────────────────────────────────────────")

		for _, dev := range pool.devices {
			indent := strings.Repeat("  ", dev.depth)
			name := indent + shortenDev(dev.name)

			if dev.state == "" {
				fmt.Fprintf(w, "       │ %s\n", name)
				continue
			}

			nameF := fmt.Sprintf("%-32s", name)
			stF := fmt.Sprintf("%-8s", dev.state)
			rdF := fmt.Sprintf("%4s", dev.read)
			wrF := fmt.Sprintf("%4s", dev.write)
			ckF := fmt.Sprintf("%4s", dev.cksum)
			slF := fmt.Sprintf("%4s", dev.slow)

			if dev.state != "ONLINE" {
				stF = ansiRed + stF + ansiReset
			}
			for _, p := range []*string{&rdF, &wrF, &ckF, &slF} {
				v := strings.TrimSpace(*p)
				if v != "0" && v != "-" && v != "" {
					*p = ansiRed + *p + ansiReset
				}
			}

			notes := ""
			if dev.notes != "" {
				n := strings.Replace(dev.notes, "(non-allocating)", "(non-alloc)", 1)
				notes = " " + n
			}

			fmt.Fprintf(w, "       │ %s %s %s %s %s %s%s\n",
				nameF, stF, rdF, wrF, ckF, slF, notes)
		}
		fmt.Fprintln(w)
	}
}
