#!/usr/bin/env bash
# Usage: hack/bench/compare.sh <base-ref> [head-ref] [pkg...]
# Env: COUNT (default 10), BENCH (default .), MAX_DELTA (percent, default 20), MAX_P (default 0.01)
set -euo pipefail

base=$1; head=${2:-HEAD}; shift $(( $# > 1 ? 2 : 1 ))
pkgs=("$@")
if [ ${#pkgs[@]} -eq 0 ]; then
  mapfile -t pkgs < <(git grep -l '^func Benchmark' -- 'pkg/**/*_test.go' | xargs -n1 dirname | sort -u | sed 's|^|./|')
fi
COUNT=${COUNT:-10}; BENCH=${BENCH:-.}; MAX_DELTA=${MAX_DELTA:-20}; MAX_P=${MAX_P:-0.01}
out=$(mktemp -d)
root=$(git rev-parse --show-toplevel)
git -C "$root" worktree add -q --detach "$out/base" "$base"
git -C "$root" worktree add -q --detach "$out/head" "$head"
trap 'git -C "$root" worktree remove --force "$out/base"; git -C "$root" worktree remove --force "$out/head"' EXIT

for ((i = 0; i < COUNT / 2; i++)); do
  for side in base head; do
    (cd "$out/$side" && go test -tags=test_performance -run='^$' -bench="$BENCH" -benchmem -count=2 "${pkgs[@]}") >> "$out/$side.txt"
  done
done

benchstat -format csv base="$out/base.txt" head="$out/head.txt" > "$out/stat.csv"
benchstat base="$out/base.txt" head="$out/head.txt" | tee "$out/stat.txt"
echo "raw results in $out"

awk -F, -v max="$MAX_DELTA" -v maxp="$MAX_P" '
  /^,/ { unit = $2; next }
  $1 == "geomean" || NF < 7 { next }
  (unit == "sec/op" || unit == "B/op" || unit == "allocs/op") && $6 ~ /^\+[0-9.]+%$/ {
    d = substr($6, 2) + 0; split($7, pv, "[= ]"); p = pv[2] + 0
    if (d >= max && p <= maxp) { print "REGRESSION " $1 " " unit " " $6 " " $7; bad = 1 }
  }
  END { exit bad }' "$out/stat.csv"
