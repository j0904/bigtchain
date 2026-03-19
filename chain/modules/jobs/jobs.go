// Package jobs manages the AI job pipeline: commit-reveal, single-server
// assignment, output commitment, and event emission for observer protocolling.
package jobs

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

// JobState tracks the lifecycle of a single job across the slot.
type JobState int

const (
	JobCommitted  JobState = iota // CommitTx received, awaiting reveal
	JobRevealed                   // RevealTx verified, server routing
	JobServed                     // Serving validator committed output hash
	JobDisputed                   // Dispute filed; in dispute/vote window
	JobReverted                   // Block reverted after successful dispute
	JobFailed                     // Server missed deadline
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
	Server        string              `json:"server"`     // VRF-elected serving validator
	Commitment    *types.OutputCommitment `json:"commitment"` // single server commitment
}

// DisputeRecord tracks an open dispute with consensus vote.
type DisputeRecord struct {
	JobID           string `json:"job_id"`
	ServingValidator string `json:"serving_validator"`
	DisputerAddr    string `json:"disputer_addr"`
	DisputeType     string `json:"dispute_type"` // "user" or "observer"
	ExpiresAtSlot   int64  `json:"expires_at_slot"`  // end of justification window
	VoteDeadline    int64  `json:"vote_deadline"`     // end of vote window
	Resolved        bool   `json:"resolved"`
	VotesUphold     int64  `json:"votes_uphold"`      // stake-weighted uphold votes
	VotesDismiss    int64  `json:"votes_dismiss"`     // stake-weighted dismiss votes
	Voters          map[string]bool `json:"voters"`    // tracks who already voted
	Immediate       bool   `json:"immediate"`          // true if cryptographically provable (no vote needed)
}

var (
	jobPrefix     = []byte("job/")
	disputePrefix = []byte("dispute/")
)

func jobKey(id string) []byte      { return append(jobPrefix, []byte(id)...) }
func disputeKey(jobID string) []byte {
	return []byte(fmt.Sprintf("dispute/%s", jobID))
}

// Module manages the job pipeline.
type Module struct {
	store *store.Store
}

// New creates a new jobs module.
func New(s *store.Store) *Module { return &Module{store: s} }

// Commit registers a job request in the commit phase.
// server is the VRF-elected serving validator for this slot.
func (m *Module) Commit(req types.JobRequest, slot int64, server string) error {
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
		Server:     server,
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

// AddCommitment records the output commitment from the single serving validator.
// Returns error if the committer is not the elected server.
func (m *Module) AddCommitment(c types.OutputCommitment) error {
	j, err := m.GetJob(c.JobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %s not found", c.JobID)
	}
	if j.State != JobRevealed {
		return fmt.Errorf("job %s not in revealed state", c.JobID)
	}

	// Only the VRF-elected serving validator can commit.
	if c.ValidatorAddr != j.Server {
		return fmt.Errorf("validator %s is not the elected server for job %s (expected %s)", c.ValidatorAddr, c.JobID, j.Server)
	}

	// Equivocation check: reject duplicate commitment from the server.
	if j.Commitment != nil {
		return fmt.Errorf("equivocation: duplicate commitment from server %s for job %s", c.ValidatorAddr, c.JobID)
	}

	j.Commitment = &c
	j.State = JobServed
	return m.putJob(j)
}

// FinaliseSlot sweeps all jobs in the given slot that have not reached JobServed
// and marks them as JobFailed (server missed deadline). Returns the server addr
// if they committed, and whether they missed.
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
		if j.Commitment != nil {
			committed = append(committed, j.Server)
		} else if j.State == JobRevealed {
			missed[j.Server] = true
			j.State = JobFailed
			_ = m.putJob(&j)
		}
		return true
	})
	return
}

// OpenDispute creates a dispute record for a job.
func (m *Module) OpenDispute(jobID, serverAddr, disputerAddr, disputeType string, currentSlot int64, immediate bool) error {
	d := &DisputeRecord{
		JobID:            jobID,
		ServingValidator: serverAddr,
		DisputerAddr:     disputerAddr,
		DisputeType:      disputeType,
		ExpiresAtSlot:    currentSlot + types.DisputeWindow,
		VoteDeadline:     currentSlot + types.DisputeWindow + types.VoteWindow,
		Resolved:         immediate, // immediately resolved if cryptographically provable
		Immediate:        immediate,
		Voters:           make(map[string]bool),
	}
	// Mark the job as disputed
	j, err := m.GetJob(jobID)
	if err != nil {
		return err
	}
	if j != nil {
		j.State = JobDisputed
		if err := m.putJob(j); err != nil {
			return err
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return m.store.Set(disputeKey(jobID), data)
}

// CastVote records a validator's vote on a dispute. Returns error if already voted.
func (m *Module) CastVote(jobID, voterAddr, vote string, voterStake int64) error {
	d, err := m.GetDispute(jobID)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("no dispute found for job %s", jobID)
	}
	if d.Resolved {
		return fmt.Errorf("dispute for job %s already resolved", jobID)
	}
	if d.Voters[voterAddr] {
		return fmt.Errorf("validator %s already voted on dispute for job %s", voterAddr, jobID)
	}
	d.Voters[voterAddr] = true
	switch vote {
	case "uphold":
		d.VotesUphold += voterStake
	case "dismiss":
		d.VotesDismiss += voterStake
	default:
		return fmt.Errorf("invalid vote: %s (expected uphold or dismiss)", vote)
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return m.store.Set(disputeKey(jobID), data)
}

// ResolveDispute marks a dispute as resolved.
func (m *Module) ResolveDispute(jobID string) error {
	d, err := m.GetDispute(jobID)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("no dispute found for job %s", jobID)
	}
	d.Resolved = true
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return m.store.Set(disputeKey(jobID), data)
}

// RevertJob marks a job as reverted (block reversion). Returns the job for re-queuing.
func (m *Module) RevertJob(jobID string) (*Job, error) {
	j, err := m.GetJob(jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %s not found", jobID)
	}
	j.State = JobReverted
	j.Commitment = nil
	if err := m.putJob(j); err != nil {
		return nil, err
	}
	return j, nil
}

// GetDispute retrieves an open dispute record.
func (m *Module) GetDispute(jobID string) (*DisputeRecord, error) {
	data, err := m.store.Get(disputeKey(jobID))
	if err != nil || data == nil {
		return nil, err
	}
	var d DisputeRecord
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListOpenDisputes returns all unresolved disputes that have passed their vote deadline.
func (m *Module) ListOpenDisputes(currentSlot int64) ([]*DisputeRecord, error) {
	var disputes []*DisputeRecord
	err := m.store.Scan(disputePrefix, func(_, val []byte) bool {
		var d DisputeRecord
		if err := json.Unmarshal(val, &d); err != nil {
			return false
		}
		if !d.Resolved && currentSlot >= d.VoteDeadline {
			disputes = append(disputes, &d)
		}
		return true
	})
	return disputes, err
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

// ComputeOutputHash computes the canonical output hash that the serving validator must commit to.
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
