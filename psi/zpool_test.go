package main

import "testing"

func TestShortenDev(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Seagate USB
		{"usb-Seagate_Expansion_HDD_00000000ABCD1234-0:0", "Seagate:ABCD1234"},
		{"usb-Seagate_Expansion_HDD_0000000012345678-0:0", "Seagate:12345678"},
		// NVMe with SN model
		{"nvme-WD_BLACK_SN770_2TB_12ABCD-part1", "SN770:12ABCD:p1"},
		{"nvme-WD_BLACK_SN770_2TB_ABCDEF", "SN770:ABCDEF"},
		// NVMe without SN (last 15 chars of the part after "nvme-")
		{"nvme-Some_Other_Drive_1234567890ABCDEF", "…234567890ABCDEF"},
		// WWN
		{"wwn-0x1234567890abcdef", "wwn:bcdef"},
		{"wwn-0x1234567890abcdef-part2", "wwn:bcdef:p2"},
		// Short name passthrough
		{"sda", "sda"},
		{"mirror-0", "mirror-0"},
		// Long name truncation (last 27 chars)
		{"some-very-long-device-name-that-exceeds-28chars", "…e-name-that-exceeds-28chars"},
	}
	for _, tt := range tests {
		got := shortenDev(tt.input)
		if got != tt.want {
			t.Errorf("shortenDev(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCondenseScan(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			"empty",
			nil,
			"",
		},
		{
			"resilver in progress with percent",
			[]string{
				"resilver in progress since Mon Jan  1 00:00:00 2024",
				"123G scanned at 1.23G/s, 45.6G issued at 456M/s, 500G total",
				"45.12% done, no estimated completion time",
			},
			"resilver 45.12% (45.6G), no est.",
		},
		{
			"scrub in progress",
			[]string{
				"scrub in progress since Mon Jan  1 00:00:00 2024",
				"100G scanned, 50.00% done",
			},
			"scrub 50.00%",
		},
		{
			"scrub done with errors",
			[]string{
				"scrub repaired 0B in 01:23:45 with 0 errors on Sun Jan  1 00:00:00 2024",
			},
			"scrub done 01:23:45, 0 err",
		},
		{
			"resilver done",
			[]string{
				"resilver repaired 1.5G in 00:10:30 with 3 errors on Sun Jan  1 00:00:00 2024",
			},
			"resilver done 00:10:30, 3 err",
		},
		{
			"unknown scan type truncated",
			[]string{
				"some very long scan status message that goes on and on and does not match known patterns at all",
			},
			"some very long scan status message that goes on an…",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := condenseScan(tt.lines)
			if got != tt.want {
				t.Errorf("condenseScan() = %q, want %q", got, tt.want)
			}
		})
	}
}
