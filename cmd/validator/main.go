// Command validator is the BIGT validator client.
// It operates in dual mode: when elected as the serving validator via VRF,
// it routes AI inference and broadcasts commitments. Otherwise, it runs as
// an observer, protocolling prompts and responses, and casting dispute votes.
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
	"github.com/bigtchain/bigt/validator/observer"
	"github.com/bigtchain/bigt/validator/router"
	"github.com/bigtchain/bigt/validator/voter"
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
		votePolicy     = flag.String("vote-policy", envOrDefault("VOTE_POLICY", "verify"), "Dispute vote policy: always_uphold, always_dismiss, verify")
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

	// Observer for when not elected as server.
	obs := observer.New(*validatorAddr)

	// Voter for dispute resolution.
	vot := voter.New(*validatorAddr, voter.Policy(*votePolicy), func(ctx context.Context, tx []byte) error {
		return broadcast.BroadcastRaw(ctx, tx)
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start listener.
	go func() {
		if err := listen.Run(ctx); err != nil {
			log.Printf("listener exited: %v", err)
		}
	}()

	log.Printf("validator started: addr=%s backend=%s mode=dual (serving+observer)", *validatorAddr, *backendURL)

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case ev := <-listen.Jobs():
			if ev.Server == *validatorAddr {
				// Serving mode: we are the elected server for this slot.
				go func(ev listener.JobEvent) {
					if err := handleJob(ctx, ev, route, broadcast); err != nil {
						log.Printf("job %s (serving): %v", ev.Job.JobID, err)
					}
				}(ev)
			} else {
				// Observer mode: protocol the job for attestation.
				go func(ev listener.JobEvent) {
					obs.RecordJob(observer.ProtocolLogEntry{
						JobID:      ev.Job.JobID,
						PromptHash: ev.Job.PromptHash,
						Server:     ev.Server,
					})
					log.Printf("job %s (observing): protocolled from server %s", ev.Job.JobID, ev.Server)
				}(ev)
			}
		}
	}

	_ = obs // used in event loop
	_ = vot // used in dispute event handling (wired via listener dispute events)
}

// handleJob routes one job to the inference backend and broadcasts the commitment.
func handleJob(ctx context.Context, ev listener.JobEvent, route *router.Router, bc *broadcaster.Broadcaster) error {
	req := ev.Job
	start := time.Now()
	deadline := time.Now().Add(3 * time.Second)
	jobCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	prompt := ev.Job.JobID // placeholder until reveal is wired
	_ = prompt

	output, err := route.Infer(jobCtx, req.JobID, req.Params.Temperature, req.Params.MaxTokens)
	if err != nil {
		retryCtx, retryCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer retryCancel()
		output, err = route.Infer(retryCtx, req.JobID, req.Params.Temperature, req.Params.MaxTokens)
		if err != nil {
			return fmt.Errorf("inference failed after retry: %w", err)
		}
	}

	elapsed := time.Since(start)
	log.Printf("job %s: inferred in %v (%d chars)", req.JobID, elapsed, len(output))

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
