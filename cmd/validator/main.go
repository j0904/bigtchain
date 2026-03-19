// Command validator is the BIGT validator router client.
// It listens for assigned jobs, routes them to an inference backend, and
// broadcasts signed output commitments to the chain.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bigtchain/bigt/validator/broadcaster"
	"github.com/bigtchain/bigt/validator/listener"
	"github.com/bigtchain/bigt/validator/router"
)

func main() {
	var (
		chainRPC       = flag.String("chain-rpc", envOrDefault("BIGT_RPC", "http://localhost:26657"), "CometBFT RPC endpoint")
		backendURL     = flag.String("backend-url", envOrDefault("BACKEND_URL", "https://api.together.xyz/v1"), "Inference backend URL")
		backendAPIKey  = flag.String("backend-api-key", os.Getenv("BACKEND_API_KEY"), "Inference backend API key")
		backendModel   = flag.String("backend-model", envOrDefault("BACKEND_MODEL", "meta-llama/Llama-3.3-70B-Instruct-Turbo"), "Model ID")
		validatorAddr  = flag.String("validator-addr", os.Getenv("VALIDATOR_ADDR"), "Validator address")
		blsPubKey      = flag.String("bls-pub-key", os.Getenv("BLS_PUB_KEY"), "BLS public key (hex)")
		blsPrivKey     = flag.String("bls-priv-key", os.Getenv("BLS_PRIV_KEY"), "BLS private key (hex)")
	)
	flag.Parse()

	if *validatorAddr == "" {
		fmt.Fprintln(os.Stderr, "error: --validator-addr is required")
		os.Exit(1)
	}

	listen, err := listener.New(*chainRPC, *validatorAddr)
	if err != nil {
		log.Fatalf("listener: %v", err)
	}

	route := router.New(router.Config{
		URL:    *backendURL,
		APIKey: *backendAPIKey,
		Model:  *backendModel,
	})

	broadcast := broadcaster.New(broadcaster.Config{
		ChainRPC:      *chainRPC,
		ValidatorAddr: *validatorAddr,
		BLSPubKey:     *blsPubKey,
		BLSPrivKeyHex: *blsPrivKey,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start listener.
	go func() {
		if err := listen.Run(ctx); err != nil {
			log.Printf("listener exited: %v", err)
		}
	}()

	log.Printf("validator router started: addr=%s backend=%s", *validatorAddr, *backendURL)

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case ev := <-listen.Jobs():
			go func(ev listener.JobEvent) {
				if err := handleJob(ctx, ev, route, broadcast); err != nil {
					log.Printf("job %s: %v", ev.Job.JobID, err)
				}
			}(ev)
		}
	}
}

// handleJob routes one job to the inference backend and broadcasts the commitment.
// It single-retries on backend failure, then gives up rather than submitting late.
func handleJob(ctx context.Context, ev listener.JobEvent, route *router.Router, bc *broadcaster.Broadcaster) error {
	req := ev.Job
	start := time.Now()
	// Hard deadline: 3s of the 4s window (leave 1s for broadcast).
	deadline := time.Now().Add(3 * time.Second)
	jobCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Prompt is not in JobRequest (commit phase); it arrives via reveal.
	// In production the listener would provide it after on-chain reveal.
	prompt := ev.Job.JobID // placeholder until reveal is wired
	_ = prompt

	output, err := route.Infer(jobCtx, req.JobID, req.Params.Temperature, req.Params.MaxTokens)
	if err != nil {
		// Single retry with 500ms timeout.
		retryCtx, retryCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer retryCancel()
		output, err = route.Infer(retryCtx, req.JobID, req.Params.Temperature, req.Params.MaxTokens)
		if err != nil {
			return fmt.Errorf("inference failed after retry: %w", err)
		}
	}

	elapsed := time.Since(start)
	log.Printf("job %s: inferred in %v (%d chars)", req.JobID, elapsed, len(output))

	// Derive slot from context (simplified: use 0 as placeholder; real impl reads from block header).
	if err := bc.Broadcast(ctx, req.JobID, 0, output); err != nil {
		return fmt.Errorf("broadcast commitment: %w", err)
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
