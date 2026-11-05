package anchor

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Networks maps network names to passphrases and default RPC endpoints.
var Networks = map[string]struct{ Passphrase, RPC string }{
	"testnet": {network.TestNetworkPassphrase, "https://soroban-testnet.stellar.org"},
	"mainnet": {network.PublicNetworkPassphrase, "https://mainnet.sorobanrpc.com"},
}

// maxFee is the most the service bids per anchor transaction, in stroops.
// It is charged only under surge pricing; normally the fee is 100.
const maxFee = 10_000

// Stellar anchors roots from one account through Stellar RPC.
type Stellar struct {
	Key        *keypair.Full
	Passphrase string
	RPC        *rpcclient.Client
	Validity   time.Duration // transaction time bounds
}

// Account implements Chain.
func (s *Stellar) Account() string { return s.Key.Address() }

// Build implements Chain: one bump_sequence to 0 (a no-op) with the root as
// MEMO_HASH.
func (s *Stellar) Build(ctx context.Context, root [32]byte) (string, string, error) {
	acct, err := s.RPC.LoadAccount(ctx, s.Key.Address())
	if err != nil {
		return "", "", fmt.Errorf("anchor account %s: %w", s.Key.Address(), err)
	}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        acct,
		IncrementSequenceNum: true,
		BaseFee:              maxFee,
		Memo:                 txnbuild.MemoHash(root),
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(int64(s.Validity / time.Second))},
		Operations:           []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 0}},
	})
	if err != nil {
		return "", "", err
	}
	if tx, err = tx.Sign(s.Passphrase, s.Key); err != nil {
		return "", "", err
	}
	env, err := tx.Base64()
	if err != nil {
		return "", "", err
	}
	hash, err := tx.HashHex(s.Passphrase)
	return env, hash, err
}

// Send implements Chain.
func (s *Stellar) Send(ctx context.Context, envelope string) error {
	res, err := s.RPC.SendTransaction(ctx, protocol.SendTransactionRequest{Transaction: envelope})
	if err != nil {
		return err
	}
	switch res.Status {
	case "PENDING", "DUPLICATE":
		return nil
	default:
		return fmt.Errorf("sendTransaction: %s %s", res.Status, res.ErrorResultXDR)
	}
}

// Status implements Chain.
func (s *Stellar) Status(ctx context.Context, hash string) (Status, error) {
	res, err := s.RPC.GetTransaction(ctx, protocol.GetTransactionRequest{Hash: hash})
	if err != nil {
		return Status{}, err
	}
	return Status{State: res.Status, Ledger: res.Ledger, ClosedAt: time.Unix(res.LedgerCloseTime, 0).UTC()}, nil
}
