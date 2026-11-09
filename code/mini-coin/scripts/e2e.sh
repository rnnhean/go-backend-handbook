#!/usr/bin/env bash
set -euo pipefail
go build -o /tmp/mini-coin ./cmd/server
MINICOIN_CONFIG=config/dev.json /tmp/mini-coin &
pid=$!
trap 'kill "$pid"' EXIT
sleep 1
response=$(curl -fsS -X POST http://127.0.0.1:8081/v1/recharges \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer demo-token' \
  -H 'X-Request-Id: e2e-1' \
  -d '{"req_id":"e2e-req-1","amount_cent":10,"channel":"manual"}')
printf '%s\n' "$response"
printf '%s' "$response" | grep -q '"code":"OK"'
printf '%s' "$response" | grep -q '"request_id":"e2e-1"'
