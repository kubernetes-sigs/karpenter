#!/usr/bin/env bash
set -euo pipefail

BEFORE="${1:?usage: benchmark-gate.sh <before/results.txt> <after/results.txt>}"
AFTER="${2:?usage: benchmark-gate.sh <before/results.txt> <after/results.txt>}"
OUT_DIR="${BENCH_GATE_OUT_DIR:-.}"
THRESHOLD_PCT="${BENCH_GATE_THRESHOLD_PCT:-20}"
MAX_P="${BENCH_GATE_MAX_P:-0.05}"
UNGATED="${BENCH_GATE_UNGATED:-^(Scheduling[0-9]+|RespectPreferences|IgnorePreferences)(/|\$)}"

CSV="${OUT_DIR}/cmp.csv"
TXT="${OUT_DIR}/cmp.txt"
: >"${TXT}"
: >"${CSV}"

if ! command -v benchstat >/dev/null 2>&1; then
  echo "::error::benchstat is not on PATH"
  exit 2
fi

for f in "${BEFORE}" "${AFTER}"; do
  if [[ ! -f "${f}" ]]; then
    echo "::error::missing ${f}"
    exit 2
  fi
  grep -E '^(Benchmark|goos:|goarch:|pkg:)' "${f}" >"${f}.clean" || true
done

if ! grep -q '^Benchmark' "${AFTER}.clean"; then
  echo "::error::no benchmark result lines in ${AFTER}"
  exit 2
fi

if ! grep -q '^Benchmark' "${BEFORE}.clean"; then
  echo "::warning::${BEFORE} holds no benchmark result lines, so every benchmark in this package is new and there is nothing to compare"
  echo "the baseline runs no benchmarks in this package; every benchmark here is new in this PR" >"${TXT}"
  exit 0
fi

benchstat "${BEFORE}.clean" "${AFTER}.clean" >"${TXT}"
benchstat -format csv "${BEFORE}.clean" "${AFTER}.clean" >"${CSV}" 2>/dev/null

exec awk -F, \
  -v threshold="${THRESHOLD_PCT}" \
  -v maxp="${MAX_P}" \
  -v ungated="${UNGATED}" '
BEGIN { unit = ""; gated = 0; regressed = 0; broken = 0; seenallocs = 0 }
{ sub(/\r$/, "") }
NF <= 1 { next }
$1 == "" && $3 == "CI" { unit = $2; next }
$1 == "" { next }
$1 == "geomean" { next }
{
  if (unit != "allocs/op" && unit != "B/op") next
  n = NF
  name = $1
  for (i = 2; i <= n - 6; i++) name = name "," $i
  sub(/-[0-9]+$/, "", name)
  if (name ~ ungated) next
  if (unit == "allocs/op") seenallocs = 1
  if (n < 7) {
    if ($2 != "") {
      printf "::error::%s %s: in the baseline but absent after this PR\n", name, unit
      broken++
    } else {
      printf "::warning::%s %s: new benchmark, no baseline to compare\n", name, unit
    }
    next
  }
  base = $(n - 5) + 0
  tip = $(n - 3) + 0
  delta = $(n - 1)
  havep = 0
  pv = 0
  if (match($n, /p=[0-9.]+/)) { havep = 1; pv = substr($n, RSTART + 2, RLENGTH - 2) + 0 }
  gated++
  if (delta == "~") next
  if (delta == "?") {
    if (tip > base) {
      printf "::error::%s %s: %.0f to %.0f, benchstat reports no comparable ratio\n", name, unit, base, tip
      regressed++
    }
    next
  }
  pct = delta + 0
  if (pct < threshold) next
  if (!havep) {
    printf "::error::%s %s: %s with no p value reported\n", name, unit, delta
    regressed++
    next
  }
  if (pv < maxp) {
    printf "::error::%s %s: %s (p=%.3f)\n", name, unit, delta, pv
    regressed++
  }
}
END {
  if (!seenallocs) { printf "::error::the comparison carries no gated allocs/op rows\n"; broken++ }
  if (gated == 0) { printf "::error::no benchmark rows were gated\n"; broken++ }
  printf "gated %d row(s) at >=%s%% allocs/op or B/op with p<%s; %d regression(s)\n", gated, threshold, maxp, regressed
  if (broken > 0) exit 2
  if (regressed > 0) exit 1
  exit 0
}' "${CSV}"
