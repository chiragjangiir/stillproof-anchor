#!/usr/bin/env bash
# Stillproof end to end on Stellar testnet: fund an anchor account, run the
# service, sign a file with a development key, submit it, wait for the
# receipt, and verify the file against it through Horizon.
#
# Run from this repository with ../stillproof checked out beside it. Needs Go
# and curl. Uses port 8080.
set -euo pipefail

WORK=$(mktemp -d)
PIDS=()
trap 'kill "${PIDS[@]}" 2>/dev/null || true; wait 2>/dev/null; rm -rf "$WORK"' EXIT
EXE=$(go env GOEXE)
go build -o "$WORK/stillproof-anchor$EXE" ./cmd/stillproof-anchor
(cd ../stillproof && go build -o "$WORK/stillproof$EXE" ./cmd/stillproof)
winpath() { command -v cygpath >/dev/null && cygpath -m "$1" || echo "$1"; }

echo "==> creating and funding an anchor account"
read -r ANCHOR_SECRET ACCOUNT < <("$WORK/stillproof-anchor$EXE" keygen)
export ANCHOR_SECRET
curl -fsS "https://friendbot.stellar.org/?addr=$ACCOUNT" >/dev/null
echo "    $ACCOUNT"

"$WORK/stillproof-anchor$EXE" -db "$(winpath "$WORK/anchor.db")" 2>"$WORK/anchor.log" &
PIDS+=($!)
for _ in $(seq 30); do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break; sleep 1; done

echo "==> signing a photo with a development key"
head -c 4096 /dev/urandom >"$WORK/photo.jpg"
"$WORK/stillproof$EXE" keygen "$(winpath "$WORK/device.key")"
"$WORK/stillproof$EXE" sign --key "$(winpath "$WORK/device.key")" "$(winpath "$WORK/photo.jpg")" >"$WORK/claim.txt"
ID=$(curl -fsS -X POST -H 'Content-Type: text/plain' --data-binary @"$WORK/claim.txt" http://127.0.0.1:8080/v1/claims | sed 's/.*"id":"\([0-9a-f]*\)".*/\1/')
echo "    claim $ID"

echo "==> waiting for the receipt"
for _ in $(seq 60); do
  code=$(curl -s -o "$WORK/receipt.json" -w '%{http_code}' "http://127.0.0.1:8080/v1/receipts/$ID")
  [ "$code" = 200 ] && break
  sleep 2
done
[ "$code" = 200 ] || { echo "no receipt; service log:" >&2; cat "$WORK/anchor.log" >&2; exit 1; }
grep -o '"tx":"[0-9a-f]*"' "$WORK/receipt.json"

echo "==> verifying through Horizon"
"$WORK/stillproof$EXE" verify "$(winpath "$WORK/photo.jpg")" "$(winpath "$WORK/receipt.json")"

echo "==> a different file must fail"
head -c 4096 /dev/urandom >"$WORK/other.jpg"
if "$WORK/stillproof$EXE" verify "$(winpath "$WORK/other.jpg")" "$(winpath "$WORK/receipt.json")"; then
  echo "unexpected: another file verified" >&2
  exit 1
fi
