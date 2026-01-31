package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// All latency buckets in order
var allLatencies = []string{
	"1ns", "3ns", "7ns", "15ns", "31ns", "63ns", "127ns", "255ns", "511ns",
	"1us", "2us", "4us", "8us", "16us", "32us", "65us", "131us", "262us", "524us",
	"1ms", "2ms", "4ms", "8ms", "16ms", "33ms", "67ms", "134ms", "268ms", "536ms",
	"1s", "2s", "4s", "8s", "17s", "34s", "68s", "137s",
}

// Latency buckets to display
var displayLatencies = allLatencies

// SMR "large" starts at 134ms, others at 33ms
var smrLargeStart = "134ms"
var defaultLargeStart = "33ms"

type DeviceData struct {
	Name    string
	Latency map[string]int64
	Total   int64
}

func main() {
	// Run zpool iostat -wv
	cmd := exec.Command("zpool", "iostat", "-wv")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating pipe: %v\n", err)
		os.Exit(1)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running zpool: %v\n", err)
		os.Exit(1)
	}

	var devices []DeviceData
	var currentDevice *DeviceData

	headerPattern := regexp.MustCompile(`^(\S+)\s+total_wait`)
	latencyPattern := regexp.MustCompile(`^([\d\.]+(?:ns|us|ms|s))\s+(.+)`)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()

		if matches := headerPattern.FindStringSubmatch(line); matches != nil {
			if currentDevice != nil {
				devices = append(devices, *currentDevice)
			}
			currentDevice = &DeviceData{
				Name:    matches[1],
				Latency: make(map[string]int64),
			}
			continue
		}

		if currentDevice != nil {
			if matches := latencyPattern.FindStringSubmatch(line); matches != nil {
				latency := matches[1]
				fields := strings.Fields(matches[2])
				if len(fields) >= 4 {
					r := parseCount(fields[2])
					w := parseCount(fields[3])
					currentDevice.Latency[latency] = r + w
					currentDevice.Total += r + w
				}
			}
		}
	}

	if currentDevice != nil {
		devices = append(devices, *currentDevice)
	}

	cmd.Wait()

	// Build device map by short name
	devMap := make(map[string]*DeviceData)
	for i := range devices {
		d := &devices[i]
		short := shortName(d.Name)
		if short != "" {
			devMap[short] = d
		}
	}

	// Define columns: sda, nvme0, nvme1, sdb, sdc, sdd, sde, sdf, sdg
	type ColDef struct {
		label string
		keys  []string
		isSMR bool
	}
	cols := []ColDef{
		{"sda", []string{"sda-p4", "sda-p5", "sda-p6"}, false},
		{"nvme0", []string{"nvme0"}, false},
		{"nvme1", []string{"nvme1"}, false},
		{"sdb", []string{"sdb"}, true},
		{"sdc", []string{"sdc"}, true},
		{"sdd", []string{"sdd"}, true},
		{"sde", []string{"sde"}, true},
		{"sdf", []string{"sdf"}, true},
		{"sdg", []string{"sdg"}, true},
	}

	// Calculate totals per column
	type ColData struct {
		label   string
		total   int64
		latency map[string]int64
		isSMR   bool
	}
	var colData []ColData

	for _, col := range cols {
		cd := ColData{label: col.label, latency: make(map[string]int64), isSMR: col.isSMR}
		for _, key := range col.keys {
			if d, ok := devMap[key]; ok {
				cd.total += d.Total
				for lat, count := range d.Latency {
					cd.latency[lat] += count
				}
			}
		}
		colData = append(colData, cd)
	}

	// Print header
	fmt.Printf("%-8s", "latency")
	for _, c := range colData {
		fmt.Printf(" %7s", c.label)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("-", 8+8*len(colData)))

	// Print rows for all latency buckets
	for _, lat := range displayLatencies {
		fmt.Printf("%-8s", lat)
		for _, c := range colData {
			if c.total == 0 {
				fmt.Printf(" %7s", "-")
				continue
			}
			pct := float64(c.latency[lat]) / float64(c.total) * 100
			if pct >= 0.01 {
				fmt.Printf(" %6.2f%%", pct)
			} else {
				fmt.Printf(" %7s", "-")
			}
		}
		fmt.Println()
	}

	// Print total_large row
	fmt.Println(strings.Repeat("-", 8+8*len(colData)))
	fmt.Printf("%-8s", "LARGE")
	for _, c := range colData {
		if c.total == 0 {
			fmt.Printf(" %7s", "-")
			continue
		}
		startLat := defaultLargeStart
		if c.isSMR {
			startLat = smrLargeStart
		}
		largeSum := int64(0)
		inLarge := false
		for _, lat := range allLatencies {
			if lat == startLat {
				inLarge = true
			}
			if inLarge {
				largeSum += c.latency[lat]
			}
		}
		pct := float64(largeSum) / float64(c.total) * 100
		fmt.Printf(" %6.2f%%", pct)
	}
	fmt.Println()

	// Print total ops row
	fmt.Printf("%-8s", "total")
	for _, c := range colData {
		if c.total == 0 {
			fmt.Printf(" %7s", "-")
		} else if c.total >= 1000000 {
			fmt.Printf(" %6.1fM", float64(c.total)/1000000)
		} else if c.total >= 1000 {
			fmt.Printf(" %6.1fK", float64(c.total)/1000)
		} else {
			fmt.Printf(" %7d", c.total)
		}
	}
	fmt.Println()

	fmt.Printf("\nLARGE: flash (sda/nvme) >= 33ms (~4x), SMR (sdb-sdg) >= 134ms\n")
}

func parseCount(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0
	}

	multiplier := int64(1)
	if strings.HasSuffix(s, "K") {
		multiplier = 1000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "M") {
		multiplier = 1000000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "G") {
		multiplier = 1000000000
		s = s[:len(s)-1]
	}

	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(multiplier))
}

func shortName(name string) string {
	// wwn-0x5002538da01ceedd-part4/5/6 -> sda-p4/p5/p6
	if strings.HasPrefix(name, "wwn-0x5002538da01ceedd-part") {
		part := strings.TrimPrefix(name, "wwn-0x5002538da01ceedd-part")
		return "sda-p" + part
	}
	// nvme-WD_BLACK_SN770_2TB_245077404326-part1 -> nvme0
	if strings.Contains(name, "245077404326") {
		return "nvme0"
	}
	// nvme-WD_BLACK_SN770_2TB_24493Z401591 -> nvme1
	if strings.Contains(name, "24493Z401591") {
		return "nvme1"
	}
	// USB Seagates -> sdb, sdc, sdd, sde, sdf, sdg
	usbMap := map[string]string{
		"NT17FBP5": "sdc",
		"NT17FBQC": "sdd",
		"NT17FC6F": "sde",
		"NT17FC7Z": "sdf",
		"NT17DHQR": "sdg",
		// sdb - add serial when known
	}
	for serial, sd := range usbMap {
		if strings.Contains(name, serial) {
			return sd
		}
	}
	return ""
}
