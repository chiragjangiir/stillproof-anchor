#!/usr/bin/env bash
# Stillproof end to end on Stellar testnet: fund an anchor account, run the
# service, sign a file with a development key, submit it, wait for the
# receipt, and verify the file against it through Horizon.
#
# With exnode and externalize (v0.3.0 or later, for externalize/bundle/v2) on
# PATH it also runs exnode, waits for the validator proof (SPEC.md §8.2: the
# ledger and the next one must reach a history archive, about 5 to 10
# minutes), verifies against the validators, and checks that
# tampered receipts fail. DEMO_OUT=DIR keeps the photo and the v1 receipt.
#
# Run from this repository with ../stillproof (or $STILLPROOF_DIR) checked
# out. Needs Go and curl. Uses ports 8080 and 8081.
set -euo pipefail

WORK=$(mktemp -d)
PIDS=()
trap 'kill "${PIDS[@]}" 2>/dev/null || true; wait 2>/dev/null; rm -rf "$WORK"' EXIT
EXE=$(go env GOEXE)
go build -o "$WORK/stillproof-anchor$EXE" ./cmd/stillproof-anchor
(cd "${STILLPROOF_DIR:-../stillproof}" && go build -o "$WORK/stillproof$EXE" ./cmd/stillproof)
winpath() { command -v cygpath >/dev/null && cygpath -m "$1" || echo "$1"; }

echo "==> creating and funding an anchor account"
read -r ANCHOR_SECRET ACCOUNT < <("$WORK/stillproof-anchor$EXE" keygen)
export ANCHOR_SECRET
curl -fsS "https://friendbot.stellar.org/?addr=$ACCOUNT" >/dev/null
echo "    $ACCOUNT"

EXNODE_URL=
if command -v exnode >/dev/null && command -v externalize >/dev/null; then
  exnode serve --network testnet --addr 127.0.0.1:8081 2>"$WORK/exnode.log" &
  PIDS+=($!)
  EXNODE_URL=http://127.0.0.1:8081
fi
"$WORK/stillproof-anchor$EXE" -db "$(winpath "$WORK/anchor.db")" -exnode "$EXNODE_URL" 2>"$WORK/anchor.log" &
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

[ -n "$EXNODE_URL" ] || { echo "==> exnode or externalize not on PATH: skipping the validator proof"; exit 0; }
echo "==> waiting for the validator proof (the ledger must reach a history archive)"
for _ in $(seq 120); do
  curl -fsS -o "$WORK/receipt.json" "http://127.0.0.1:8080/v1/receipts/$ID"
  grep -q '"v":1' "$WORK/receipt.json" && break
  sleep 10
done
grep -q '"v":1' "$WORK/receipt.json" || { echo "no proof; service log:" >&2; cat "$WORK/anchor.log" >&2; exit 1; }
"$WORK/stillproof$EXE" verify "$(winpath "$WORK/photo.jpg")" "$(winpath "$WORK/receipt.json")" | tee "$WORK/verdict.txt"
grep -q "validators' signatures" "$WORK/verdict.txt"
[ -z "${DEMO_OUT:-}" ] || cp "$WORK/photo.jpg" "$WORK/receipt.json" "$DEMO_OUT"

# Tampered copies must fail at the anchor step, where Horizon can't overrule
# a proof that was checked.
mustfail() {
  if "$WORK/stillproof$EXE" verify "$(winpath "$WORK/photo.jpg")" "$(winpath "$2")" | tee "$WORK/verdict.txt"; then
    echo "unexpected: $1 verified" >&2
    exit 1
  fi
  grep -q "^FAILED at anchor: validator proof:" "$WORK/verdict.txt"
}
echo "==> a receipt naming another anchor account must fail"
sed "s/\"account\":\"$ACCOUNT\"/\"account\":\"GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H\"/" "$WORK/receipt.json" >"$WORK/forged.json"
mustfail "a forged account" "$WORK/forged.json"
echo "==> a proof with an edited ledger header must fail"
sed -E 's/("ledger":")A/\1B/; t; s/("ledger":")./\1A/' "$WORK/receipt.json" >"$WORK/forged.json"
mustfail "an edited header" "$WORK/forged.json"
