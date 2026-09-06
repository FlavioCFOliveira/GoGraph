#!/bin/zsh
# rmp #2763 interleaved A/B. No `set -e`: the exit code of each go test is read
# and recorded rather than aborting the run.
S=/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/30c8f7b2-7300-4908-9f28-8fc3ea0e6d43/scratchpad/rmp2763
R=/Users/flaviocfo/dev/xumiga/GoGraph
ARM_A=$1; ARM_B=$2; OUT_A=$3; OUT_B=$4; ROUNDS=$5; TAG=$6
BENCH='Benchmark(ShortestPath|AllShortestPaths)_'
: > $OUT_A ; : > $OUT_B
: > $S/ab/loadavg_$TAG.log
: > $S/ab/exits_$TAG.log
run_one () {
  arm=$1; out=$2; i=$3
  cp $S/ab/${arm}_sp.go    $R/cypher/exec/shortest_path.go
  cp $S/ab/${arm}_bidir.go $R/cypher/exec/shortest_path_bidir.go
  cp $S/ab/${arm}_profile.go $R/cypher/exec/profile.go
  echo "round=$i arm=$arm pre  $(uptime | sed 's/.*load average[s]*: //')" >> $S/ab/loadavg_$TAG.log
  go test -count=1 -run XXX -bench "$BENCH" -benchmem -benchtime=300ms $R/cypher/exec/ >> $out 2>&1
  ec=$?
  echo "round=$i arm=$arm exit=$ec" >> $S/ab/exits_$TAG.log
  echo "round=$i arm=$arm post $(uptime | sed 's/.*load average[s]*: //')" >> $S/ab/loadavg_$TAG.log
}
for i in $(seq 1 $ROUNDS); do
  run_one $ARM_A $OUT_A $i
  run_one $ARM_B $OUT_B $i
done
cp $S/ab/head_sp.go    $R/cypher/exec/shortest_path.go
cp $S/ab/head_bidir.go $R/cypher/exec/shortest_path_bidir.go
cp $S/ab/head_profile.go $R/cypher/exec/profile.go
echo DONE
