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
	DisputeWindow   = int64(7)  // slots for justification
	VoteWindow      = int64(3)  // slots for consensus vote after dispute window
	CommitDeadline  = int64(4)  // seconds into slot

	// Slashing amounts (basis points of bond)
	SlashMismatch  = int64(1000)  // 10% — observer-detected mismatch
	SlashMalicious = int64(10000) // 100% — user dispute or equivocation

	// Single-server architecture: 1 serving validator per slot
	ServersPerJob  = 1
	BFTCommitteeSize = 128
	InactivityThresholdEpochs = 4
	UnbondingSlots = 21 * 24 * EpochSlots / EpochSlots * EpochSlots // 21 days in slots

	// Revenue sharing (basis points of total job revenue)
	RewardServingBPS  = int64(5000) // 50% to serving validator
	RewardObserverBPS = int64(3000) // 30% to observer pool
	RewardDelegatorBPS = int64(1500) // 15% to delegators
	RewardBountyBPS   = int64(500)  // 5% to dispute bounty reserve

	// Dispute vote threshold: 2/3+ of active stake (in basis points)
	DisputeVoteThresholdBPS = int64(6667) // 66.67%

	// Subscription plans (monthly, in uBIGT)
	SubscriptionBasicMonthly = int64(10_000_000)   // 10 BIGT/month — 100 jobs/month
	SubscriptionProMonthly   = int64(50_000_000)   // 50 BIGT/month — 1,000 jobs/month
	SubscriptionEnterpriseMonthly = int64(200_000_000) // 200 BIGT/month — unlimited

	SubscriptionBasicJobs      = int64(100)
	SubscriptionProJobs        = int64(1_000)
	SubscriptionEnterpriseJobs = int64(0) // 0 means unlimited

	SubscriptionDurationSlots = EpochSlots * 30 // ~30 days
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
	JobID           string `json:"job_id"`
	ValidatorAddr   string `json:"validator_addr"`
	PlaintextOutput string `json:"plaintext_output"`
}

// ObserverDisputeTx is submitted by an observer who re-executed inference and found a mismatch.
type ObserverDisputeTx struct {
	JobID             string   `json:"job_id"`
	ObserverAddr      string   `json:"observer_addr"`
	ObserverOutputHash HexBytes `json:"observer_output_hash"`
	Signature         HexBytes `json:"signature"`
}

// DisputeVoteTx is cast by a validator to uphold or dismiss a dispute.
type DisputeVoteTx struct {
	JobID         string `json:"job_id"`
	VoterAddr     string `json:"voter_addr"`
	Vote          string `json:"vote"` // "uphold" or "dismiss"
	Signature     HexBytes `json:"signature"`
}

// ProtocolAttestation is submitted by observer validators at epoch boundary.
type ProtocolAttestation struct {
	Epoch          int64    `json:"epoch"`
	ObserverAddr   string   `json:"observer_addr"`
	JobsObserved   int64    `json:"jobs_observed"`
	JobsVerified   int64    `json:"jobs_verified"`
	Signature      HexBytes `json:"signature"`
}

// ---------------------------------------------------------------------------
// Account & Subscription types
// ---------------------------------------------------------------------------

// Account represents a user's on-chain balance.
type Account struct {
	Address string `json:"address"`
	Balance int64  `json:"balance"` // uBIGT
	Nonce   int64  `json:"nonce"`
}

// SubscriptionPlan identifies a tier.
type SubscriptionPlan string

const (
	PlanBasic      SubscriptionPlan = "basic"
	PlanPro        SubscriptionPlan = "pro"
	PlanEnterprise SubscriptionPlan = "enterprise"
)

// Subscription tracks a user's active monthly subscription.
type Subscription struct {
	UserAddr    string           `json:"user_addr"`
	Plan        SubscriptionPlan `json:"plan"`
	StartSlot   int64            `json:"start_slot"`
	ExpiresSlot int64            `json:"expires_slot"`
	JobsUsed    int64            `json:"jobs_used"`
	JobsLimit   int64            `json:"jobs_limit"` // 0 = unlimited
	PaidAmount  int64            `json:"paid_amount"` // uBIGT charged
	AutoRenew   bool             `json:"auto_renew"`
}

// DepositTx credits uBIGT to a user account (e.g. from L1 bridge or faucet).
type DepositTx struct {
	UserAddr string `json:"user_addr"`
	Amount   int64  `json:"amount"`
}

// SubscribeTx creates or renews a monthly subscription.
type SubscribeTx struct {
	UserAddr  string           `json:"user_addr"`
	Plan      SubscriptionPlan `json:"plan"`
	AutoRenew bool             `json:"auto_renew"`
}

// CancelSubscriptionTx cancels auto-renewal of a subscription.
type CancelSubscriptionTx struct {
	UserAddr string `json:"user_addr"`
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
	TxJobRequest        TxType = "job_request"
	TxReveal            TxType = "reveal"
	TxCommitment        TxType = "commitment"
	TxDispute           TxType = "dispute"
	TxObserverDispute   TxType = "observer_dispute"
	TxDisputeVote       TxType = "dispute_vote"
	TxJustification     TxType = "justification"
	TxProtocolAttestation TxType = "protocol_attestation"
	TxRegValidator      TxType = "register_validator"
	TxDelegate          TxType = "delegate"
	TxUndelegate        TxType = "undelegate"
	TxUnjail            TxType = "unjail"
	TxProposeModel      TxType = "propose_model"
	TxApproveModel      TxType = "approve_model"
	TxDeposit           TxType = "deposit"
	TxSubscribe         TxType = "subscribe"
	TxCancelSubscription TxType = "cancel_subscription"
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
