package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cmtURL := flag.String("cmt", "http://localhost:26657", "CometBFT RPC URL")
	l1URL := flag.String("l1", "http://localhost:8545", "Ethereum L1 JSON-RPC URL")
	contract := flag.String("contract", "", "CheckpointAnchor contract address (0x…)")
	keyHex := flag.String("key", "", "32-byte relayer private key as hex (no 0x prefix)")
	epochSlots := flag.Uint64("epoch-slots", 14400, "Epoch size in slots/blocks")
	flag.Parse()

	if *contract == "" || *keyHex == "" {
		fmt.Fprintln(os.Stderr, "error: --contract and --key are required")
		flag.Usage()
		os.Exit(1)
	}

	privKey, err := ParseHexKey(*keyHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bad --key: %v\n", err)
		os.Exit(1)
	}

	submitter := NewHTTPSubmitter(*l1URL, *contract)
	r := New(Config{
		CMTRPCURL:    *cmtURL,
		EpochSlots:   *epochSlots,
		PrivKey:      privKey,
		PollInterval: 5 * time.Second,
	}, submitter)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("relayer started: CMT=%s L1=%s contract=%s\n", *cmtURL, *l1URL, *contract)
	if err := r.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "relayer exited: %v\n", err)
		os.Exit(1)
	}
}
