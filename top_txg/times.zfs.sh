DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for t in otime qtime wtime stime; do echo "by $t";  "$DIR/top_txgs.sh" $t; done
