#!/usr/bin/env bash
set -euo pipefail

PKG="${PKG:-./pkg/host}"
BENCH="${BENCH:-.}"
COUNT="${COUNT:-1}"
BENCHTIME="${BENCHTIME:-}"
OUT="${OUT:-}"

args=(go test -run '^$' -bench "$BENCH" -benchmem -count "$COUNT")
if [[ -n "$BENCHTIME" ]]; then
  args+=(-benchtime "$BENCHTIME")
fi
args+=("$PKG")

if [[ -n "$OUT" ]]; then
  "${args[@]}" | tee "$OUT"
else
  "${args[@]}"
fi
