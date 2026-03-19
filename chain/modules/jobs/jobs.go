// Package jobs manages the AI job pipeline: commit-reveal, worker assignment,
// output commitments, and majority-agreement checking.
package jobs

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

// JobState tracks the lifecycle of a single job across the 3-phase slot.
type JobState int

const (
	JobCommitted  JobState = iota // CommitTx received, awaiting reveal
	JobRevealed                   // RevealTx verified, workers routing
	JobAgreed                     // 2-of-3 workers agree; finalised
	JobDisputed                   // Commitment mismatch; dispute window open
	JobFailed                     // Workers missed deadline or no quorum
)

// Job represents a full job record.
type Job struct {
	JobID         string              `json:"job_id"`
	ModelID       string              `json:"model_id"`
	PromptHash    types.HexBytes      `json:"prompt_hash"`
	UserAddr      string              `json:"user_addr"`
	Prompt        string              `json:"prompt"`    // filled after reveal
	Params        types.JobParams     `json:"params"`
	State         JobState            `json:"state"`
	Slot          int64               `json:"slot"`
	Workers       []string            `json:"workers"`    // elected worker validator addresses
	Commitments   []types.OutputCommitment `json:"commitments"`
	AgreedHash    types.HexBytes      `json:"agreed_hash"` // majority-agreed output hash
}

// DisputeRecord tracks an open dispute.
type DisputeRecord struct {
	JobID          string `json:"job_id"`
	ValidatorAddr  string `json:"validator_addr"`
	ExpiresAtSlot  int64  `json:"expires_at_slot"`
	Resolved       bool   `json:"resolved"`
}

var (
	jobPrefix     = []byte("job/")
	disputePrefix = []byte("dispute/")
)

func jobKey(id string) []byte      { return append(jobPrefix, []byte(id)...) }
func disputeKey(jobID, valAddr string) []byte {
	return []byte(fmt.Sprintf("dispute/%s/%s", jobID, valAddr))
}

// Module manages the job pipeline.
type Module struct {
	store *store.Store
}

// New creates a new jobs module.
func New(s *store.Store) *Module { return &Module{store: s} }

// Commit registers a job request in the commit phase.
// promptHash = keccak256(prompt || nonce) provided by the user.
func (m *Module) Commit(req types.JobRequest, slot int64, workers []string) error {
	existing, err := m.GetJob(req.JobID)
	if err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf("job %s already exists", req.JobID)
	}
	j := &Job{
		JobID:      req.JobID,
		ModelID:    req.ModelID,
		PromptHash: req.PromptHash,
		UserAddr:   req.UserAddr,
		Params:     req.Params,
		State:      JobCommitted,
		Slot:       slot,
		Workers:    workers,
	}
	return m.putJob(j)
}

// Reveal verifies the plaintext reveal and advances job to JobRevealed.
func (m *Module) Reveal(rev types.RevealTx) error {
	j, err := m.GetJob(rev.JobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %s not found", rev.JobID)
	}
	if j.State != JobCommitted {
		return fmt.Errorf("job %s not in committed state (got %d)", rev.JobID, j.State)
	}
	if err := rev.Validate(j.PromptHash); err != nil {
		return fmt.Errorf("reveal validation failed for job %s: %w", rev.JobID, err)
	}
	j.Prompt = rev.Prompt
	j.State = JobRevealed
	return m.putJob(j)
}

// AddCommitment records an output commitment from a worker validator.
// Returns (agreed, error). agreed is true when 2-of-3 workers have submitted
// matching hashes.
func (m *Module) AddCommitment(c types.OutputCommitment) (bool, error) {
	j, err := m.GetJob(c.JobID)
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, fmt.Errorf("job %s not found", c.JobID)
	}
	if j.State != JobRevealed {
		return false, fmt.Errorf("job %s not in revealed state", c.JobID)
	}

	// Verify this validator is an elected worker.
	isWorker := false
	for _, w := range j.Workers {
		if w == c.ValidatorAddr {
			isWorker = true
			break
		}
	}
	if !isWorker {
		return false, fmt.Errorf("validator %s is not an elected worker for job %s", c.ValidatorAddr, c.JobID)
	}

	// Check for duplicate commitment from same validator.
	for _, existing := range j.Commitments {
		if existing.ValidatorAddr == c.ValidatorAddr {
			return false, fmt.Errorf("duplicate commitment from validator %s for job %s", c.ValidatorAddr, c.JobID)
		}
	}

	j.Commitments = append(j.Commitments, c)

	// Check majority agreement (2-of-3).
	agreed, agreedHash := majority(j.Commitments)
	if agreed {
		j.State = JobAgreed
		j.AgreedHash = agreedHash
	}

	if err := m.putJob(j); err != nil {
		return false, err
	}
	return agreed, nil
}

// FinaliseSlot sweeps all jobs in the given slot that have not reached JobAgreed
// and marks them as JobFailed (workers missed deadline). Returns addrs of
// validators who committed (to reset inactivity) and those who missed.
func (m *Module) FinaliseSlot(slot int64) (committed []string, missed map[string]bool, err error) {
	missed = make(map[string]bool)
	err = m.store.Scan(jobPrefix, func(_, val []byte) bool {
		var j Job
		if innerErr := json.Unmarshal(val, &j); innerErr != nil {
			return false
		}
		if j.Slot != slot {
			return true
		}
		for _, w := range j.Workers {
			found := false
			for _, c := range j.Commitments {
				if c.ValidatorAddr == w {
					found = true
					break
				}
			}
			if found {
				committed = append(committed, w)
			} else {
				missed[w] = true
			}
		}
		if j.State == JobRevealed {
			// Check for mismatch in commitments (dispute trigger)
			if hasMismatch(j.Commitments) {
				j.State = JobDisputed
			} else {
				j.State = JobFailed
			}
			_ = m.putJob(&j)
		}
		return true
	})
	return
}

// OpenDispute creates a dispute record for a mismatched commitment.
func (m *Module) OpenDispute(jobID, valAddr string, currentSlot int64) error {
	d := &DisputeRecord{
		JobID:         jobID,
		ValidatorAddr: valAddr,
		ExpiresAtSlot: currentSlot + types.DisputeWindow,
		Resolved:      false,
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return m.store.Set(disputeKey(jobID, valAddr), data)
}

// GetDispute retrieves an open dispute record.
func (m *Module) GetDispute(jobID, valAddr string) (*DisputeRecord, error) {
	data, err := m.store.Get(disputeKey(jobID, valAddr))
	if err != nil || data == nil {
		return nil, err
	}
	var d DisputeRecord
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ResolveDispute marks a dispute as resolved.
func (m *Module) ResolveDispute(jobID, valAddr string) error {
	d, err := m.GetDispute(jobID, valAddr)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("no dispute found for job %s validator %s", jobID, valAddr)
	}
	d.Resolved = true
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return m.store.Set(disputeKey(jobID, valAddr), data)
}

// GetJob retrieves a job by ID. Returns (nil, nil) if not found.
func (m *Module) GetJob(id string) (*Job, error) {
	data, err := m.store.Get(jobKey(id))
	if err != nil || data == nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// ComputeOutputHash computes the canonical output hash that workers must commit to.
// output_hash = keccak256(output_tokens || job_id || validator_pubkey)
func ComputeOutputHash(outputTokens []byte, jobID, validatorPubKey string) []byte {
	return types.Keccak256(outputTokens, []byte(jobID), []byte(validatorPubKey))
}

func (m *Module) putJob(j *Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return m.store.Set(jobKey(j.JobID), data)
}

// majority returns true and the winning hash if 2+ commitments have the same hash.
func majority(cs []types.OutputCommitment) (bool, types.HexBytes) {
	counts := make(map[string]int)
	hashes := make(map[string]types.HexBytes)
	for _, c := range cs {
		key := fmt.Sprintf("%x", c.OutputHash)
		counts[key]++
		hashes[key] = c.OutputHash
	}
	for k, cnt := range counts {
		if cnt >= 2 {
			return true, hashes[k]
		}
	}
	return false, nil
}

// hasMismatch returns true if at least two commitments with different hashes exist.
func hasMismatch(cs []types.OutputCommitment) bool {
	if len(cs) < 2 {
		return false
	}
	ref := fmt.Sprintf("%x", cs[0].OutputHash)
	for _, c := range cs[1:] {
		if fmt.Sprintf("%x", c.OutputHash) != ref {
			return true
		}
	}
	return false
}
