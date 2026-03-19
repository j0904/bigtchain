// Package types defines the core protocol types for the BIGT chain.
package types

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/sha3"
)

// Token amounts are represented in BIGT micro-units (1 BIGT = 1_000_000 uBIGT).
const (
	MinStake        = int64(5_000_000_000)   // 5,000 BIGT in uBIGT
	MaxValidatorPct = int64(5)               // 5% of total stake
	SlotSeconds     = int64(6)
	EpochSlots      = int64(14_400)
	DisputeWindow   = int64(7) // slots
	CommitDeadline  = int64(4) // seconds into slot

	// Slashing amounts (basis points of bond)
	SlashMismatch  = int64(1000) // 10%
	SlashMalicious = int64(10000) // 100%

	WorkersPerJob  = 3
	BFTCommitteeSize = 128
	InactivityThresholdEpochs = 4
	UnbondingSlots = 21 * 24 * EpochSlots / EpochSlots * EpochSlots // 21 days in slots
)

// Keccak256 computes keccak256 of the input data.
func Keccak256(data ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	return h.Sum(nil)
}

// HexBytes is a []byte that marshals to/from hex JSON.
type HexBytes []byte

func (h HexBytes) MarshalJSON() ([]byte, error) {
	return json.Marshal("0x" + hex.EncodeToString(h))
}

func (h *HexBytes) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if len(s) >= 2 && s[:2] == "0x" {
		s = s[2:]
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return err
	}
	*h = b
	return nil
}

func (h HexBytes) String() string { return "0x" + hex.EncodeToString(h) }

// ---------------------------------------------------------------------------
// Job types
// ---------------------------------------------------------------------------

// JobRequest is submitted by a user to request AI inference.
type JobRequest struct {
	JobID      string   `json:"job_id"`
	ModelID    string   `json:"model_id"`
	PromptHash HexBytes `json:"prompt_hash"` // keccak256(prompt || nonce) — commit phase
	UserAddr   string   `json:"user_addr"`
	Params     JobParams `json:"params"`
}

// JobParams carries model inference parameters.
type JobParams struct {
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

// RevealTx reveals the plaintext prompt after the commit phase.
type RevealTx struct {
	JobID  string `json:"job_id"`
	Prompt string `json:"prompt"`
	Nonce  string `json:"nonce"`
}

// Validate checks that the reveal matches the original commitment.
func (r RevealTx) Validate(committed []byte) error {
	h := Keccak256([]byte(r.Prompt), []byte(r.Nonce))
	for i, b := range h {
		if i >= len(committed) || b != committed[i] {
			return fmt.Errorf("reveal does not match commitment")
		}
	}
	return nil
}

// OutputCommitment is submitted by a worker validator after routing the job.
type OutputCommitment struct {
	JobID          string   `json:"job_id"`
	ValidatorAddr  string   `json:"validator_addr"`
	OutputHash     HexBytes `json:"output_hash"` // keccak256(output_tokens || job_id || validator_pubkey)
	Slot           int64    `json:"slot"`
	Signature      HexBytes `json:"signature"` // BLS signature over output_hash
}

// DisputeTx is submitted by a user who received an output not matching the commitment.
type DisputeTx struct {
	JobID          string `json:"job_id"`
	ValidatorAddr  string `json:"validator_addr"`
	PlaintextOutput string `json:"plaintext_output"`
}

// JustificationTx is submitted by a validator to justify a mismatched commitment.
type JustificationTx struct {
	JobID         string `json:"job_id"`
	ValidatorAddr string `json:"validator_addr"`
	Reason        string `json:"reason"`
	Evidence      HexBytes `json:"evidence"` // e.g. on-chain model update proof
}

// ---------------------------------------------------------------------------
// Transaction envelope
// ---------------------------------------------------------------------------

// TxType identifies the kind of transaction.
type TxType string

const (
	TxJobRequest      TxType = "job_request"
	TxReveal          TxType = "reveal"
	TxCommitment      TxType = "commitment"
	TxDispute         TxType = "dispute"
	TxJustification   TxType = "justification"
	TxRegValidator    TxType = "register_validator"
	TxDelegate        TxType = "delegate"
	TxUndelegate      TxType = "undelegate"
	TxUnjail          TxType = "unjail"
	TxProposeModel    TxType = "propose_model"
	TxApproveModel    TxType = "approve_model"
)

// Tx is the generic transaction envelope used in DeliverTx.
type Tx struct {
	Type    TxType          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Sender  string          `json:"sender"`
}

// ---------------------------------------------------------------------------
// Block / slot helpers
// ---------------------------------------------------------------------------

// SlotPhase returns the sub-phase (1, 2, or 3) of a timestamp within a slot.
// blockTime is the wall-clock second offset within the slot (0-5).
func SlotPhase(offsetSeconds int64) int {
	switch {
	case offsetSeconds < 1:
		return 1 // Commit phase
	case offsetSeconds < 4:
		return 2 // Route and respond
	default:
		return 3 // BFT commit
	}
}
