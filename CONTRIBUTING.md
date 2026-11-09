# Contributing

`stillproof-anchor` is the Stillproof anchoring service: it accepts signed
claims, batches them into a Merkle tree and anchors each root on Stellar. It
takes part in the [Stellar Wave](https://www.drips.network/wave/stellar)
program; Wave issues are labeled with their complexity. The ground rules for
every Stillproof repository are in the
[organization guide](https://github.com/stillproof-labs/.github/blob/main/CONTRIBUTING.md).

## Setup

```sh
git clone https://github.com/stillproof-labs/stillproof-anchor && cd stillproof-anchor
go test ./...
```

Go 1.26 or later (`go.mod` pins it). Tests run offline: the Stellar chain and
exnode are faked, and each test uses its own SQLite database in a temporary
directory.

## Before you open a PR

These are the checks CI runs (`.github/workflows/ci.yml`):

```sh
gofmt -l .                 # must print nothing
go vet ./...
go test -race -count=1 ./...
golangci-lint run
govulncheck ./...
```

CI runs the tests on Linux, macOS and Windows.

## Testnet demo

`scripts/testnet-demo.sh` runs the whole flow on Stellar testnet: it funds an
anchor account through friendbot, runs the service, signs and submits a file,
waits for the receipt and verifies it through Horizon. It needs Go, curl and
the core repository checked out beside this one (`../stillproof`, or set
`STILLPROOF_DIR`), and uses ports 8080 and 8081. With `exnode` and
`externalize` (v0.3.0 or later) on `PATH` it also waits for the validator
proof, which takes 5 to 10 minutes. Testnet is free; never use a mainnet
secret for development. Changes to batching or anchoring should include the
demo's output in the PR.

## The protocol

Routes, receipts and the anchor transaction follow
[SPEC.md](https://github.com/stillproof-labs/stillproof/blob/main/SPEC.md) in
the core repository. A change to any of them lands there first.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org): `feat(api): …`,
`fix(store): …`, `docs: …`, `test: …`.
