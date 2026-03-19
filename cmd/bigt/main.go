// Command bigt runs a BIGT chain node.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	cmtconfig "github.com/cometbft/cometbft/config"
	cmtlog "github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/node"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"

	"github.com/bigtchain/bigt/chain/app"
	"github.com/bigtchain/bigt/chain/genesis"
)

func main() {
	var (
		homeDir     = flag.String("home", os.ExpandEnv("$HOME/.bigt"), "chain home directory")
		initGenesis = flag.Bool("init", false, "initialise genesis and exit")
		showNodeID  = flag.Bool("show-node-id", false, "print node ID and exit")
	)
	flag.Parse()

	if *initGenesis {
		if err := doInit(*homeDir); err != nil {
			fmt.Fprintf(os.Stderr, "init: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("genesis initialised at", *homeDir)
		return
	}

	if *showNodeID {
		cfg := cmtconfig.DefaultConfig()
		cfg.SetRoot(*homeDir)
		nodeKey, err := p2p.LoadNodeKey(cfg.NodeKeyFile())
		if err != nil {
			fmt.Fprintf(os.Stderr, "load node key: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(nodeKey.ID())
		return
	}

	if err := runNode(*homeDir); err != nil {
		fmt.Fprintf(os.Stderr, "node error: %v\n", err)
		os.Exit(1)
	}
}

func doInit(homeDir string) error {
	cfg := cmtconfig.DefaultConfig()
	cfg.SetRoot(homeDir)

	if err := os.MkdirAll(homeDir+"/config", 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(homeDir+"/data", 0o700); err != nil {
		return err
	}

	// Generate CometBFT private validator key and state.
	privval.LoadOrGenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile())

	// Generate CometBFT node key.
	if _, err := p2p.LoadOrGenNodeKey(cfg.NodeKeyFile()); err != nil {
		return err
	}

	// Write default CometBFT config.toml.
	cmtconfig.WriteConfigFile(cfg.RootDir+"/config/config.toml", cfg)

	// Write default app genesis.
	g := genesis.Default()
	return g.Save(homeDir + "/config/genesis.json")
}

func runNode(homeDir string) error {
	// Load chain config.
	cfg := cmtconfig.DefaultConfig()
	cfg.SetRoot(homeDir)
	cfg.Consensus.TimeoutCommit = 6_000_000_000 // 6 seconds in nanoseconds
	cfg.Consensus.CreateEmptyBlocks = true
	cfg.Consensus.CreateEmptyBlocksInterval = 6_000_000_000

	// Allow P2P configuration via environment variables.
	if peers := os.Getenv("BIGT_PERSISTENT_PEERS"); peers != "" {
		cfg.P2P.PersistentPeers = peers
	}
	if os.Getenv("BIGT_ALLOW_DUPLICATE_IP") == "true" {
		cfg.P2P.AllowDuplicateIP = true
	}
	if os.Getenv("BIGT_ADDR_BOOK_STRICT") == "false" {
		cfg.P2P.AddrBookStrict = false
	}
	cfg.P2P.ListenAddress = "tcp://0.0.0.0:26656"
	cfg.RPC.ListenAddress = "tcp://0.0.0.0:26657"

	// Load genesis to verify it's valid.
	if _, err := genesis.Load(homeDir + "/config/genesis.json"); err != nil {
		return fmt.Errorf("load genesis: %w", err)
	}

	// Create application.
	bigt, err := app.New(homeDir + "/data/bigt.db")
	if err != nil {
		return fmt.Errorf("create app: %w", err)
	}

	// Load or generate private validator key.
	pv := privval.LoadOrGenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile())
	nodeKey, err := p2p.LoadOrGenNodeKey(cfg.NodeKeyFile())
	if err != nil {
		return fmt.Errorf("load node key: %w", err)
	}

	// Override genesis doc loader to use our genesis.
	logger := cmtlog.NewTMLogger(cmtlog.NewSyncWriter(os.Stdout))
	n, err := node.NewNode(
		cfg,
		pv,
		nodeKey,
		proxy.NewLocalClientCreator(bigt),
		node.DefaultGenesisDocProviderFunc(cfg),
		cmtconfig.DefaultDBProvider,
		node.DefaultMetricsProvider(cfg.Instrumentation),
		logger,
	)
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}

	if err := n.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	return n.Stop()
}
