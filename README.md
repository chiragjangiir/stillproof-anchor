# stillproof-anchor

The Stillproof anchoring service. It accepts signed claims from capture
apps, gathers each batch into a Merkle tree, and records the tree's root on
Stellar. Each claim then gets a receipt that anyone can verify against the
public ledger. The protocol is in
[`stillproof/SPEC.md`](https://github.com/stillproof-labs/stillproof/blob/main/SPEC.md).

- **One transaction per batch, however many claims it holds.** At the default
  of one batch every 5 seconds, anchoring costs at most about 0.17 XLM a day
  at the base fee. Empty intervals send nothing.
- **No trust in the operator for timing.** A root in ledger L proves its
  claims existed by L's close time, whoever runs the service. An operator can
  only refuse or delay.
- **No files.** The service sees only signed fingerprints, never photos.

## Proven on testnet

[`scripts/testnet-demo.sh`](scripts/testnet-demo.sh) funds an anchor account,
runs the service, signs a file with a development key, submits it, waits for
the receipt and verifies the file through Horizon:

```console
==> verifying through Horizon
VERIFIED
  unchanged since signed by key 04be4de08868afe611… (software key)
  existed no later than 2026-11-05T03:26:10Z (ledger 5565106, testnet)
  device says captured 2026-11-05T03:25:55Z
  This proves the file is unchanged and when it existed. It does not prove the scene is genuine.
==> a different file must fail
FAILED at file: the file's SHA-256 is not the one in the claim
```

**Validator proofs (SPEC.md §8.2).** Run with `-exnode URL` (or
`EXNODE_URL`; empty disables), the service fetches each anchored batch's
Externalize bundle, with the transaction set, from
`GET /v1/ledgers/{ledger}/bundle?tx=…&txset=true`, retrying until the ledger
is in a history archive. If `externalize` is on `PATH` it checks the bundle
first. Receipts then carry it as `anchor.proof`, with `v` 1. Run on testnet
on 2026-11-08 with exnode v0.2.0 and externalize v0.2.0
(`scripts/testnet-demo.sh`): tx `d772b310…5493`, ledger 5615318, proof
in the receipt under 4 minutes after the ledger closed, and

```console
VERIFIED
  existed no later than 2026-11-08T01:10:32Z (ledger 5615318, testnet)
  anchor checked against the validators' signatures
==> a receipt naming another anchor account must fail
FAILED at anchor: validator proof: the bundle's transaction set has no transaction d772b310… from that account
==> a proof with an edited ledger header must fail
FAILED at anchor: validator proof: rejected: ledger 5615318: header hash does not match header contents
```

## Run it

```sh
docker run --rm --entrypoint /stillproof-anchor ghcr.io/stillproof-labs/stillproof-anchor keygen   # prints SECRET ADDRESS
docker run -d -p 8080:8080 -v stillproof:/data -e ANCHOR_SECRET=S... \
  ghcr.io/stillproof-labs/stillproof-anchor -network mainnet -app-id TEAMID.io.github.stillprooflabs.app
```

Or `go install github.com/stillproof-labs/stillproof-anchor/cmd/stillproof-anchor@latest`.
The image listens on `:8080` and keeps its SQLite database in the `/data`
volume. It runs as a non-root user.

**The anchor account** is the one `ANCHOR_SECRET` signs for. It must exist
and stay funded: on mainnet, fund it with at least the 1 XLM minimum balance
plus fees. Each batch is one transaction charged the network's base fee,
normally 100 stroops (0.00001 XLM). Under surge pricing the service bids up
to 10,000 stroops (0.001 XLM) per transaction, and never more. The account
needs no trustlines or other setup. Use it for nothing else, so its sequence
number stays the service's own.

At startup the service checks that the RPC serves the network named by
`-network` and exits if it doesn't.

| Flag | Default |
|---|---|
| `-network` | `testnet` (or `mainnet`) |
| `-rpc` | The network's public Stellar RPC: `soroban-testnet.stellar.org`, or `mainnet.sorobanrpc.com` (a third-party provider; set your own for production) |
| `-app-id` | `$ANCHOR_APP_ID`. Accepted App Attest ids, comma-separated `TEAMID.bundle` |
| `-db` | `stillproof.db` (SQLite); `/data/stillproof.db` in the image |
| `-interval` | `5s` between batches |
| `-listen` | `127.0.0.1:8080`; `:8080` in the image |

## API

Every route allows any origin (CORS), so browser verifiers can call it.

| Route | Result |
|---|---|
| `POST /v1/claims` | Body: a signed claim (`text/plain`). Returns `202 {"id"}`, or `400` if it is malformed or the signature is invalid. Resubmitting returns the same id |
| `GET /v1/receipts/{id}` | `200` with the receipt, or `202 {"status": "pending"}` until it is anchored |
| `GET /v1/receipts?sha256=<hex>` | `200 {"receipts": [...]}`: every anchored receipt for that file, oldest first. `202 {"status": "pending"}` if all are pending, `404` if unknown |
| `POST /v1/keys` | Body: `{"key": "<hex>", "attestation": {...}}`, up to 64 KB. Returns `204`, or `400` with the reason if the attestation doesn't show the key is in hardware. Registering again is a no-op |
| `GET /healthz` | `200` |

Once a key is registered, its receipts are `v` 1 and carry its attestation
(SPEC.md §8.1).

## How anchoring stays correct

- **The root never changes once cut.** A batch's root is fixed when the batch
  is created. Each anchor transaction's hash is stored before the transaction
  is sent.
- **Retries keep the same root.** If a transaction fails, or isn't seen
  before its time bounds expire, a new one is built for the same root.
- **Restarts don't double-anchor.** After a restart, the service first checks
  whether the recorded transaction landed, so it never re-anchors a batch
  that already made it.

## Development

```sh
go test -race ./...
bash scripts/testnet-demo.sh   # needs ../stillproof checked out beside this repository
```

## License

[Apache-2.0](LICENSE)
