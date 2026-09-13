#!/usr/bin/env bash
# 로컬 slowserver 를 향하는 모니터 N개짜리 설정을 만든다.
#   ./scripts/gen-bench-config.sh 50 > configs/bench-50.yaml
set -euo pipefail
n="${1:-50}"
echo "# 벤치마크용 설정 — 로컬 slowserver 를 향한 모니터 ${n}개"
echo "# 먼저 다른 터미널에서: go run ./cmd/slowserver -delay 200ms"
echo "workers: 8"
echo "monitors:"
for i in $(seq -w 1 "$n"); do
  printf '  - name: bench-%s\n    type: http\n    target: http://localhost:8080/slow\n    interval_sec: 10\n    timeout_ms: 5000\n' "$i"
done
