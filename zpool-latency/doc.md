 The tool is working correctly now with both sections:

  - INTERVAL: Shows stats from the most recent interval (populated after the first interval completes)
  - LIFETIME: Shows cumulative stats fetched directly from ZFS every 2 seconds

  The architecture:
  1. Interval stream: Long-running zpool iostat -wvv pool <interval> - skips first output (it's lifetime), uses subsequent interval snapshots
  2. Lifetime poller: Every 2s runs zpool iostat -wvv pool (no interval = cumulative from ZFS)

  This ensures accuracy by getting both interval and lifetime stats directly from ZFS rather than computing them ourselves.

  Summary of the tool:
  ./zpool-latency [-i interval] [-batch] [-disk] <pool>

  Features:
  - INTERVAL section: Per-interval stats from streaming zpool iostat
  - LIFETIME section: Cumulative stats polled directly from ZFS
  - Real-time 10 FPS display
  - Shortened device names for readability
  - Shows total_wait (queue + disk) or disk_wait (-disk flag)
