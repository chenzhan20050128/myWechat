#!/usr/bin/env bash
# Enforces module boundary rules (docs/ARCHITECTURE.md ADR-013):
#   1. platform must not import any domain package
#   2. domain modules may only import the ROOT package of other domains
#      (never sub-packages), and only platform/* otherwise
# Usage: bash scripts/check-boundaries.sh  (exit 1 on violation)
set -euo pipefail
cd "$(dirname "$0")/.."

MODULE="github.com/example/wechat"
DOMAINS=(auth user device contact conversation media group audit)
# future phases: message group group_todo moment moment_schedule favorite
#   storage_cleanup backup content_account service_session notification operator

fail=0

for p in $(go list ./internal/platform/...); do
  deps=$(go list -deps "$p" 2>/dev/null | grep "^$MODULE/internal/" || true)
  for d in $deps; do
    for dom in "${DOMAINS[@]}"; do
      if [[ "$d" == *"/internal/$dom"* || "$d" == "$MODULE/internal/$dom" ]]; then
        echo "VIOLATION: platform package $p imports domain package $d"
        fail=1
      fi
    done
  done
done

for src in "${DOMAINS[@]}"; do
  [ -d "internal/$src" ] || continue
  for p in $(go list "./internal/$src/..." 2>/dev/null); do
    deps=$(go list -deps "$p" 2>/dev/null | grep "^$MODULE/internal/" || true)
    for d in $deps; do
      # skip self and platform
      [[ "$d" == "$MODULE/internal/$src"* || "$d" == "$MODULE/internal/platform"* ]] && continue
      ok=0
      for dst in "${DOMAINS[@]}"; do
        [[ "$d" == "$MODULE/internal/$dst" ]] && ok=1 && break
      done
      if [[ $ok -eq 0 ]]; then
        echo "VIOLATION: $p imports non-root or unknown package $d (cross-domain imports must target the module root package only)"
        fail=1
      fi
    done
  done
done

if [[ $fail -ne 0 ]]; then
  echo "boundary check FAILED"
  exit 1
fi
echo "boundary check OK"
