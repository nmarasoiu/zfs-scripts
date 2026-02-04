#!/usr/bin/env bash
# Trace reads taking longer than threshold (default 1000ms)
# Useful for debugging slow pipelines (tail -f | grep | cut etc)

THRESH_MS=${1:-1000}

bpftrace -e "
tracepoint:syscalls:sys_enter_read {
    @start[tid] = nsecs;
    @comm[tid] = comm;
    @fd[tid] = args->fd;
}

tracepoint:syscalls:sys_exit_read /@start[tid]/ {
    \$lat = (nsecs - @start[tid]) / 1000000;
    if (\$lat > $THRESH_MS) {
        printf(\"SLOW %s[%d] fd=%d: %dms\\n\", @comm[tid], pid, @fd[tid], \$lat);
    }
    delete(@start[tid]);
    delete(@comm[tid]);
    delete(@fd[tid]);
}

END {
    printf(\"\\n=== Pending reads at exit ===\\n\");
    print(@comm);
    clear(@start); clear(@comm); clear(@fd);
}
"
