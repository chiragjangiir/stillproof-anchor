// Command stillproof-anchor accepts signed claims and anchors them on Stellar.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/stillproof-labs/stillproof-anchor/internal/anchor"
	"github.com/stillproof-labs/stillproof-anchor/internal/api"
	"github.com/stillproof-labs/stillproof-anchor/internal/store"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "keygen" {
		kp, err := keypair.Random()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println(kp.Seed(), kp.Address())
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve on")
	dbPath := flag.String("db", "stillproof.db", "SQLite database")
	net := flag.String("network", "testnet", "testnet or mainnet")
	rpcURL := flag.String("rpc", "", "Stellar RPC (default: the network's public endpoint)")
	interval := flag.Duration("interval", 5*time.Second, "how often to cut a batch")
	appIDs := flag.String("app-id", os.Getenv("ANCHOR_APP_ID"), "accepted App Attest ids, comma-separated TEAMID.bundle (env ANCHOR_APP_ID)")
	flag.Parse()

	n, ok := anchor.Networks[*net]
	if !ok {
		return fmt.Errorf("unknown network %q", *net)
	}
	if *rpcURL == "" {
		*rpcURL = n.RPC
	}
	key, err := keypair.ParseFull(os.Getenv("ANCHOR_SECRET"))
	if err != nil {
		return errors.New("ANCHOR_SECRET must hold the anchor account's secret key (S...)")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rpc := rpcclient.NewClient(*rpcURL, nil)
	info, err := rpc.GetNetwork(ctx)
	if err != nil {
		return fmt.Errorf("rpc %s: %w", *rpcURL, err)
	}
	if info.Passphrase != n.Passphrase {
		return fmt.Errorf("rpc %s serves %q, not %s", *rpcURL, info.Passphrase, *net)
	}
	chain := &anchor.Stellar{Key: key, Passphrase: n.Passphrase, RPC: rpc, Validity: time.Minute}
	b := &anchor.Batcher{Store: st, Chain: chain, MaxBatch: 100_000, Expiry: 90 * time.Second, Poll: time.Second, Log: log}
	go b.Run(ctx, *interval)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&api.Server{Store: st, Network: *net, Log: log, AppIDs: splitList(*appIDs)}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("serving", "addr", *listen, "network", *net, "rpc", *rpcURL, "account", key.Address(), "app_ids", *appIDs)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
