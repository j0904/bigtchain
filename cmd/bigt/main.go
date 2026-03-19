// Command bigt runs a BIGT chain node.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	cmtconfig "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/node"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"

	"github.com/bigtchain/bigt/chain/app"
	"github.com/bigtchain/bigt/chain/genesis"
)

func main() {
	var (
		homeDir    = flag.String("home", os.ExpandEnv("$HOME/.bigt"), "chain home directory")
		initGenesis = flag.Bool("init", false, "initialise genesis and exit")
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

	if err := runNode(*homeDir); err != nil {
		fmt.Fprintf(os.Stderr, "node error: %v\n", err)
		os.Exit(1)
	}
}

func doInit(homeDir string) error {
	if err := os.MkdirAll(homeDir+"/config", 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(homeDir+"/data", 0o700); err != nil {
		return err
	}
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

	// Load genesis.
	g, err := genesis.Load(homeDir + "/config/genesis.json")
	if err != nil {
		return fmt.Errorf("load genesis: %w", err)
	}
	appState, err := json.Marshal(g)
	if err != nil {
		return err
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
	_ = appState // appState used in InitChain via genesis file
	_ = g

	n, err := node.NewNode(
		cfg,
		pv,
		nodeKey,
		proxy.NewLocalClientCreator(bigt),
		node.DefaultGenesisDocProviderFunc(cfg),
		cmtconfig.DefaultDBProvider,
		node.DefaultMetricsProvider(cfg.Instrumentation),
		nil,
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
