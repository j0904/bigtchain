// Package slashing implements dispute resolution, consensus voting, and slashing.
// Under the single-server architecture, disputes are raised by users or observers,
// then resolved either immediately (cryptographic proof) or via 2/3+ stake-weighted
// consensus vote among active validators.
package slashing

import (
	"bytes"
	"fmt"

	"github.com/bigtchain/bigt/chain/modules/jobs"
	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/types"
)

// Event describes a slash that occurred.
type Event struct {
	ValidatorAddr string
	AmountSlashed int64
	Reason        string
	JobID         string
}

// Handler processes disputes, consensus votes, and equivocation.
type Handler struct {
	staking *staking.Module
	jobs    *jobs.Module
}

// New creates a new slashing handler.
func New(st *staking.Module, jm *jobs.Module) *Handler {
	return &Handler{staking: st, jobs: jm}
}

// ProcessUserDispute handles a DisputeTx submitted by a user.
// The user provides the plaintext output; we recompute its hash on-chain and
// compare to the serving validator's committed hash.
// If hashes don't match, this is cryptographically provable fraud → immediate slash + revert.
// Returns a slash event if proven; nil if dispute is invalid.
func (h *Handler) ProcessUserDispute(tx types.DisputeTx, currentSlot int64) (*Event, error) {
	j, err := h.jobs.GetJob(tx.JobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %s not found", tx.JobID)
	}

	// Single-server: check the server's commitment directly.
	if j.Commitment == nil {
		return nil, fmt.Errorf("no commitment found for job %s", tx.JobID)
	}
	if j.Commitment.ValidatorAddr != tx.ValidatorAddr {
		return nil, fmt.Errorf("validator %s is not the server for job %s", tx.ValidatorAddr, tx.JobID)
	}

	// Recompute expected hash using validator's BLS pubkey.
	v, err := h.staking.GetValidator(tx.ValidatorAddr)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("validator %s not found", tx.ValidatorAddr)
	}

	expected := jobs.ComputeOutputHash([]byte(tx.PlaintextOutput), tx.JobID, v.BLSPubKey)

	if bytes.Equal(expected, j.Commitment.OutputHash) {
		// Hashes match — dispute invalid.
		return nil, nil
	}

	// Cryptographically provable fraud → immediate slash (no vote needed).
	amount, err := h.staking.SlashBond(tx.ValidatorAddr, types.SlashMalicious)
	if err != nil {
		return nil, err
	}

	// Open an immediate dispute and revert the job.
	if err := h.jobs.OpenDispute(tx.JobID, tx.ValidatorAddr, tx.ValidatorAddr, "user", currentSlot, true); err != nil {
		return nil, err
	}
	if _, err := h.jobs.RevertJob(tx.JobID); err != nil {
		return nil, err
	}

	return &Event{
		ValidatorAddr: tx.ValidatorAddr,
		AmountSlashed: amount,
		Reason:        "user_dispute_proven",
		JobID:         tx.JobID,
	}, nil
}

// ProcessObserverDispute handles an ObserverDisputeTx from a validator who
// re-executed inference and found a mismatch. Opens a dispute vote window.
func (h *Handler) ProcessObserverDispute(tx types.ObserverDisputeTx, currentSlot int64) error {
	j, err := h.jobs.GetJob(tx.JobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %s not found", tx.JobID)
	}
	if j.Commitment == nil {
		return fmt.Errorf("no commitment to dispute for job %s", tx.JobID)
	}

	// Verify the observer is an active validator (not the server).
	observer, err := h.staking.GetValidator(tx.ObserverAddr)
	if err != nil {
		return err
	}
	if observer == nil || observer.Status != staking.StatusActive {
		return fmt.Errorf("observer %s is not an active validator", tx.ObserverAddr)
	}
	if tx.ObserverAddr == j.Server {
		return fmt.Errorf("server cannot dispute its own job")
	}

	// Open a dispute with consensus vote window; not immediately resolved.
	return h.jobs.OpenDispute(tx.JobID, j.Server, tx.ObserverAddr, "observer", currentSlot, false)
}

// CastVote records a validator's stake-weighted vote on a dispute.
func (h *Handler) CastVote(tx types.DisputeVoteTx) error {
	v, err := h.staking.GetValidator(tx.VoterAddr)
	if err != nil {
		return err
	}
	if v == nil || v.Status != staking.StatusActive {
		return fmt.Errorf("voter %s is not an active validator", tx.VoterAddr)
	}
	return h.jobs.CastVote(tx.JobID, tx.VoterAddr, tx.Vote, v.TotalStake)
}

// TallyExpiredDisputes checks disputes whose vote deadline has passed,
// tallies votes, and applies slashing/reversion if upheld.
// Returns slash events for upheld disputes.
func (h *Handler) TallyExpiredDisputes(currentSlot int64) ([]Event, error) {
	disputes, err := h.jobs.ListOpenDisputes(currentSlot)
	if err != nil {
		return nil, err
	}

	totalStake, err := h.staking.TotalStake()
	if err != nil {
		return nil, err
	}
	if totalStake == 0 {
		return nil, nil
	}

	var events []Event
	for _, d := range disputes {
		// Threshold: uphold votes > DisputeVoteThresholdBPS * totalStake / 10000
		threshold := totalStake * types.DisputeVoteThresholdBPS / 10_000
		if d.VotesUphold > threshold {
			// Dispute upheld — slash serving validator and revert job.
			amount, err := h.staking.SlashBond(d.ServingValidator, types.SlashMismatch)
			if err != nil {
				return nil, err
			}
			if _, err := h.jobs.RevertJob(d.JobID); err != nil {
				return nil, err
			}
			events = append(events, Event{
				ValidatorAddr: d.ServingValidator,
				AmountSlashed: amount,
				Reason:        "observer_dispute_upheld",
				JobID:         d.JobID,
			})
		}
		// Mark dispute as resolved regardless of outcome.
		if err := h.jobs.ResolveDispute(d.JobID); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// ProcessEquivocation handles a validator submitting two different commitments
// for the same job_id. Immediate 100% slash.
func (h *Handler) ProcessEquivocation(validatorAddr, jobID string) (*Event, error) {
	amount, err := h.staking.SlashBond(validatorAddr, types.SlashMalicious)
	if err != nil {
		return nil, err
	}
	return &Event{
		ValidatorAddr: validatorAddr,
		AmountSlashed: amount,
		Reason:        "equivocation",
		JobID:         jobID,
	}, nil
}
