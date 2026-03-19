// Package slashing implements the slashing and dispute resolution logic.
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

// Handler processes disputes and equivocation.
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
// compare to the validator's committed hash.
// Returns a slash event if the validator lied; returns nil if dispute is invalid.
func (h *Handler) ProcessUserDispute(tx types.DisputeTx) (*Event, error) {
	j, err := h.jobs.GetJob(tx.JobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %s not found", tx.JobID)
	}

	// Find the validator's commitment in this job.
	var committed types.HexBytes
	for _, c := range j.Commitments {
		if c.ValidatorAddr == tx.ValidatorAddr {
			committed = c.OutputHash
			break
		}
	}
	if committed == nil {
		return nil, fmt.Errorf("no commitment found for validator %s in job %s", tx.ValidatorAddr, tx.JobID)
	}

	// Recompute expected hash.
	// Get validator's BLS pubkey for hash computation.
	v, err := h.staking.GetValidator(tx.ValidatorAddr)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("validator %s not found", tx.ValidatorAddr)
	}

	expected := jobs.ComputeOutputHash([]byte(tx.PlaintextOutput), tx.JobID, v.BLSPubKey)

	if bytes.Equal(expected, committed) {
		// Hashes match — dispute invalid. Fee burned by caller logic.
		return nil, nil
	}

	// Validator lied: 100% slash.
	amount, err := h.staking.SlashBond(tx.ValidatorAddr, types.SlashMalicious)
	if err != nil {
		return nil, err
	}
	return &Event{
		ValidatorAddr: tx.ValidatorAddr,
		AmountSlashed: amount,
		Reason:        "user_dispute_proven",
		JobID:         tx.JobID,
	}, nil
}

// ProcessMismatch handles the case where 1-of-3 worker commitments diverged from
// the majority. Called from EndBlock when a dispute window expires without justification.
// Returns a slash event for the minority validator.
func (h *Handler) ProcessMismatch(jobID, validatorAddr string) (*Event, error) {
	amount, err := h.staking.SlashBond(validatorAddr, types.SlashMismatch)
	if err != nil {
		return nil, err
	}
	return &Event{
		ValidatorAddr: validatorAddr,
		AmountSlashed: amount,
		Reason:        "commitment_mismatch_unjustified",
		JobID:         jobID,
	}, nil
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
