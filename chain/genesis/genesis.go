// Package genesis defines the genesis state and initial configuration.
package genesis

import (
	"encoding/json"
	"os"
	"time"
)

// TotalSupply in uBIGT (100 million BIGT).
const TotalSupply = int64(100_000_000_000_000)

// ValidatorGenesis is an initial validator in the genesis file.
type ValidatorGenesis struct {
	Address     string `json:"address"`
	PubKey      string `json:"pub_key"`   // base64 ed25519 public key (consensus)
	BLSPubKey   string `json:"bls_pub_key"` // hex BLS12-381 public key (signing)
	Bond        int64  `json:"bond"`
	Commission  int64  `json:"commission_bps"` // basis points, e.g. 500 = 5%
	Moniker     string `json:"moniker"`
}

// Genesis is the root genesis document.
type Genesis struct {
	ChainID        string             `json:"chain_id"`
	GenesisTime    time.Time          `json:"genesis_time"`
	TotalSupply    int64              `json:"total_supply_ubigt"`
	Validators     []ValidatorGenesis `json:"validators"`
	MinStake       int64              `json:"min_stake_ubigt"`
	SlotSeconds    int64              `json:"slot_seconds"`
	EpochSlots     int64              `json:"epoch_slots"`
}

// Default returns a genesis document suitable for a single-node devnet.
func Default() *Genesis {
	return &Genesis{
		ChainID:     "bigt-devnet-1",
		GenesisTime: time.Now().UTC(),
		TotalSupply: TotalSupply,
		MinStake:    5_000_000_000, // 5,000 BIGT in uBIGT
		SlotSeconds: 6,
		EpochSlots:  14_400,
		Validators: []ValidatorGenesis{
			{
				Address:    "val1_address_placeholder",
				PubKey:     "val1_ed25519_pubkey_placeholder",
				BLSPubKey:  "val1_bls_pubkey_placeholder",
				Bond:       10_000_000_000, // 10,000 BIGT
				Commission: 500,            // 5%
				Moniker:    "devnet-validator-1",
			},
		},
	}
}

// Load reads a genesis JSON from disk.
// Supports both raw app genesis format and CometBFT genesis format
// (where the app genesis is nested inside the "app_state" field).
func Load(path string) (*Genesis, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Try CometBFT genesis format first (has app_state field).
	var wrapper struct {
		AppState json.RawMessage `json:"app_state"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.AppState) > 0 {
		var g Genesis
		if err := json.Unmarshal(wrapper.AppState, &g); err == nil && g.ChainID != "" {
			return &g, nil
		}
	}

	// Fall back to raw app genesis format.
	var g Genesis
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// Save writes the genesis JSON to disk.
func (g *Genesis) Save(path string) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
