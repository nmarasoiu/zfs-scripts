for t in otime qtime wtime stime; do echo "by $t";  /root/tools/zfs/top_txgs.sh $t; done
