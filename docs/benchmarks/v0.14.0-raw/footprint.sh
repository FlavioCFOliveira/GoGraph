#!/bin/bash
# footprint.sh — graph/index/count BenchmarkStoreFootprint, interleaved, at the DEFAULT
# GOMAXPROCS only.
#
# Not a ladder on purpose. The benchmark's custom `B/store` metric is derived from
# runtime.MemStats, which is PROCESS-GLOBAL, divided by b.N -- and b.N changes with
# -test.cpu. Sweeping the ladder therefore changes the DENOMINATOR, not the footprint:
# the five levels read 49984 / 107328 / 566080 / 2138945 / 8430413 B/store on the SAME
# binary. Those are not five footprints, they are one footprint divided by five
# different iteration counts. So the metric is only comparable at a fixed level.
set -u
SP=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad
OUT=$SP/raw/supp; mkdir -p "$OUT"; rm -f "$OUT"/count_* "$OUT"/exits.log "$OUT"/loadavg.log
: > "$OUT/exits.log"; : > "$OUT/loadavg.log"
for r in 1 2 3 4 5 6; do
  case $(( (r-1)%3 )) in 0) ARMS="A B B2" ;; 1) ARMS="B B2 A" ;; 2) ARMS="B2 A B" ;; esac
  for arm in $ARMS; do
    echo "round=$r arm=$arm BEFORE $(uptime)" >> "$OUT/loadavg.log"
    "$SP/bin/graph_index_count_${arm}.test" -test.run=XXXNONE \
       -test.bench='^BenchmarkStoreFootprint$' -test.benchmem -test.count=1 \
      >> "$OUT/count_${arm}.txt" 2>> "$OUT/count_${arm}.stderr"
    echo "round=$r arm=$arm EXIT=$?" >> "$OUT/exits.log"
    echo "round=$r arm=$arm AFTER  $(uptime)" >> "$OUT/loadavg.log"
  done
done
echo DONE >> "$OUT/exits.log"
