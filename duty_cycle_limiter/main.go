package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	onDuration  time.Duration
	offDuration time.Duration
	bufferSize  int64
	statsInterval time.Duration
)

func parseSize(s string) (int64, error) {
	var size int64
	var unit string
	_, err := fmt.Sscanf(s, "%d%s", &size, &unit)
	if err != nil {
		// Try without unit
		_, err = fmt.Sscanf(s, "%d", &size)
		if err != nil {
			return 0, fmt.Errorf("invalid size: %s", s)
		}
		return size, nil
	}
	switch unit {
	case "K", "k", "KB", "kb":
		size *= 1024
	case "M", "m", "MB", "mb":
		size *= 1024 * 1024
	case "G", "g", "GB", "gb":
		size *= 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown unit: %s", unit)
	}
	return size, nil
}

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.2f TB", float64(b)/TB)
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/MB)
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/KB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatRate(bytesPerSec float64) string {
	return formatBytes(int64(bytesPerSec)) + "/s"
}

func main() {
	var bufferSizeStr string

	flag.DurationVar(&onDuration, "on", 0, "ON phase duration (required, e.g., 60s, 2m)")
	flag.DurationVar(&offDuration, "off", 0, "OFF phase duration (required, e.g., 60s, 2m)")
	flag.StringVar(&bufferSizeStr, "buffer", "16M", "Buffer size (default 16M)")
	flag.DurationVar(&statsInterval, "stats", 0, "Stats reporting interval to stderr (e.g., 30s, 0 to disable)")
	flag.Parse()

	if onDuration <= 0 || offDuration <= 0 {
		fmt.Fprintf(os.Stderr, "Usage: %s --on <duration> --off <duration> [--buffer <size>] [--stats <interval>]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nExample: cat bigfile | %s --on 60s --off 60s | consumer\n", os.Args[0])
		os.Exit(1)
	}

	var err error
	bufferSize, err = parseSize(bufferSizeStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid buffer size: %v\n", err)
		os.Exit(1)
	}

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	shutdownRequested := false
	go func() {
		<-sigCh
		shutdownRequested = true
		fmt.Fprintf(os.Stderr, "\n[shutdown requested, finishing current chunk...]\n")
	}()

	// Stats tracking
	var totalBytes int64
	var cycleBytes int64
	cycleStart := time.Now()
	totalStart := time.Now()
	onPhaseStart := time.Now()
	cycleCount := 0

	var lastStatsTime time.Time
	if statsInterval > 0 {
		lastStatsTime = time.Now()
		fmt.Fprintf(os.Stderr, "[duty_cycle] on=%v off=%v buffer=%s\n", onDuration, offDuration, formatBytes(bufferSize))
	}

	buffer := make([]byte, bufferSize)

	for !shutdownRequested {
		// Read from stdin (blocks until producer has data)
		n, err := os.Stdin.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintf(os.Stderr, "Read error: %v\n", err)
			os.Exit(1)
		}

		// Write to stdout (blocks until consumer accepts)
		written := 0
		for written < n {
			w, err := os.Stdout.Write(buffer[written:n])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Write error: %v\n", err)
				os.Exit(1)
			}
			written += w
		}

		totalBytes += int64(n)
		cycleBytes += int64(n)

		// Stats reporting
		if statsInterval > 0 && time.Since(lastStatsTime) >= statsInterval {
			elapsed := time.Since(totalStart)
			rate := float64(totalBytes) / elapsed.Seconds()
			fmt.Fprintf(os.Stderr, "[%s] total: %s @ %s | cycle %d: %s\n",
				elapsed.Round(time.Second),
				formatBytes(totalBytes),
				formatRate(rate),
				cycleCount+1,
				formatBytes(cycleBytes))
			lastStatsTime = time.Now()
		}

		// Check if ON phase has expired (AFTER completing chunk transfer)
		if time.Since(onPhaseStart) >= onDuration {
			cycleCount++
			cycleDuration := time.Since(cycleStart)
			cycleRate := float64(cycleBytes) / cycleDuration.Seconds()

			if statsInterval > 0 {
				fmt.Fprintf(os.Stderr, "[cycle %d complete] %s @ %s, sleeping %v...\n",
					cycleCount, formatBytes(cycleBytes), formatRate(cycleRate), offDuration)
			}

			// OFF phase: sleep
			time.Sleep(offDuration)

			// Reset for next cycle
			cycleBytes = 0
			cycleStart = time.Now()
			onPhaseStart = time.Now()
		}
	}

	// Final stats
	totalDuration := time.Since(totalStart)
	if statsInterval > 0 || shutdownRequested {
		avgRate := float64(totalBytes) / totalDuration.Seconds()
		fmt.Fprintf(os.Stderr, "\n[done] total: %s in %v (%d cycles) @ %s avg\n",
			formatBytes(totalBytes),
			totalDuration.Round(time.Second),
			cycleCount,
			formatRate(avgRate))
	}
}
