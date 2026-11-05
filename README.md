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

## Run it

```sh
go install github.com/stillproof-labs/stillproof-anchor/cmd/stillproof-anchor@latest
stillproof-anchor keygen                  # prints SECRET ADDRESS; fund ADDRESS
ANCHOR_SECRET=S... stillproof-anchor -network testnet -listen 127.0.0.1:8080
```

| Flag | Default |
|---|---|
| `-network` | `testnet` (or `mainnet`) |
| `-rpc` | The network's public Stellar RPC |
| `-db` | `stillproof.db` (SQLite) |
| `-interval` | `5s` between batches |
| `-listen` | `127.0.0.1:8080` |

## API

| Route | Result |
|---|---|
| `POST /v1/claims` | Body: a signed claim (`text/plain`). Returns `202 {"id"}`, or `400` if it is malformed or the signature is invalid. Resubmitting returns the same id |
| `GET /v1/receipts/{id}` | `200` with the receipt, or `202 {"status": "pending"}` until it is anchored |
| `GET /healthz` | `200` |

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
