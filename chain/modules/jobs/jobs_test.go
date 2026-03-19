package jobs_test

import (
	"os"
	"testing"

	"github.com/bigtchain/bigt/chain/modules/jobs"
	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "bigt-jobs-*")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func makeJobReq(id, modelID string) types.JobRequest {
	prompt := "hello"
	nonce := "abc"
	h := types.Keccak256([]byte(prompt), []byte(nonce))
	return types.JobRequest{
		JobID:      id,
		ModelID:    modelID,
		PromptHash: h,
		UserAddr:   "user1",
		Params:     types.JobParams{Temperature: 0, MaxTokens: 128},
	}
}

func TestJobLifecycle_CommitRevealCommitment(t *testing.T) {
	m := jobs.New(newTestStore(t))
	server := "server1"

	// Phase 1: Commit
	req := makeJobReq("job-001", "llama3")
	if err := m.Commit(req, 1, server); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Phase 2: Reveal
	rev := types.RevealTx{JobID: "job-001", Prompt: "hello", Nonce: "abc"}
	if err := m.Reveal(rev); err != nil {
		t.Fatalf("reveal: %v", err)
	}

	// Single server commits output.
	hash := jobs.ComputeOutputHash([]byte("the answer is 42"), "job-001", "bls-pub-server1")
	if err := m.AddCommitment(types.OutputCommitment{
		JobID: "job-001", ValidatorAddr: "server1", OutputHash: hash, Slot: 1,
	}); err != nil {
		t.Fatalf("add commitment server: %v", err)
	}

	j, _ := m.GetJob("job-001")
	if j.State != jobs.JobServed {
		t.Errorf("expected job state Served, got %d", j.State)
	}
}

func TestReveal_WrongNonce_Rejected(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-002", "llama3")
	m.Commit(req, 1, "server1") //nolint:errcheck

	rev := types.RevealTx{JobID: "job-002", Prompt: "hello", Nonce: "wrong-nonce"}
	if err := m.Reveal(rev); err == nil {
		t.Fatal("expected reveal to fail with wrong nonce")
	}
}

func TestDuplicateCommitment_Rejected(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-003", "gpt2")
	m.Commit(req, 1, "server1") //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-003", Prompt: "hello", Nonce: "abc"}) //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte("output"), "job-003", "pub")
	m.AddCommitment(types.OutputCommitment{JobID: "job-003", ValidatorAddr: "server1", OutputHash: hash, Slot: 1}) //nolint:errcheck

	// Second commitment from same server -> should fail (equivocation).
	err := m.AddCommitment(types.OutputCommitment{JobID: "job-003", ValidatorAddr: "server1", OutputHash: hash, Slot: 1})
	if err == nil {
		t.Fatal("expected error for duplicate commitment from server")
	}
}

func TestNonServer_Commitment_Rejected(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-004", "llama3")
	m.Commit(req, 1, "server1")                                                          //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-004", Prompt: "hello", Nonce: "abc"})            //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte("output"), "job-004", "pub")
	err := m.AddCommitment(types.OutputCommitment{JobID: "job-004", ValidatorAddr: "not-server", OutputHash: hash, Slot: 1})
	if err == nil {
		t.Fatal("expected error: not-server is not the elected server")
	}
}

func TestFinaliseSlot_MarksMissedServer(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-005", "llama3")
	m.Commit(req, 5, "server1")                                                           //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-005", Prompt: "hello", Nonce: "abc"})             //nolint:errcheck

	// Server does not commit — should be marked as missed.
	_, missed, err := m.FinaliseSlot(5)
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	if !missed["server1"] {
		t.Error("expected server1 to be missed")
	}
}

func TestFinaliseSlot_ServerCommitted(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-006", "llama3")
	m.Commit(req, 6, "server1")                                                           //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-006", Prompt: "hello", Nonce: "abc"})             //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte("output"), "job-006", "pub")
	m.AddCommitment(types.OutputCommitment{JobID: "job-006", ValidatorAddr: "server1", OutputHash: hash, Slot: 6}) //nolint:errcheck

	committed, missed, err := m.FinaliseSlot(6)
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	if len(committed) == 0 {
		t.Error("expected server1 in committed list")
	}
	if missed["server1"] {
		t.Error("server1 committed, should not be missed")
	}
}

func TestDisputeVote_Lifecycle(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-007", "llama3")
	m.Commit(req, 7, "server1")                                                           //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-007", Prompt: "hello", Nonce: "abc"})             //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte("output"), "job-007", "pub")
	m.AddCommitment(types.OutputCommitment{JobID: "job-007", ValidatorAddr: "server1", OutputHash: hash, Slot: 7}) //nolint:errcheck

	// Open dispute.
	if err := m.OpenDispute("job-007", "server1", "observer1", "observer", 7, false); err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	j, _ := m.GetJob("job-007")
	if j.State != jobs.JobDisputed {
		t.Errorf("expected job state Disputed, got %d", j.State)
	}

	// Cast votes.
	if err := m.CastVote("job-007", "val1", "uphold", 1000); err != nil {
		t.Fatalf("cast vote: %v", err)
	}
	if err := m.CastVote("job-007", "val2", "dismiss", 500); err != nil {
		t.Fatalf("cast vote: %v", err)
	}

	// Duplicate vote should fail.
	if err := m.CastVote("job-007", "val1", "uphold", 1000); err == nil {
		t.Fatal("expected error for duplicate vote")
	}

	d, _ := m.GetDispute("job-007")
	if d.VotesUphold != 1000 {
		t.Errorf("expected 1000 uphold votes, got %d", d.VotesUphold)
	}
	if d.VotesDismiss != 500 {
		t.Errorf("expected 500 dismiss votes, got %d", d.VotesDismiss)
	}
}

func TestRevertJob(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-008", "llama3")
	m.Commit(req, 8, "server1")                                                           //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-008", Prompt: "hello", Nonce: "abc"})             //nolint:errcheck
	hash := jobs.ComputeOutputHash([]byte("output"), "job-008", "pub")
	m.AddCommitment(types.OutputCommitment{JobID: "job-008", ValidatorAddr: "server1", OutputHash: hash, Slot: 8}) //nolint:errcheck

	reverted, err := m.RevertJob("job-008")
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.State != jobs.JobReverted {
		t.Errorf("expected JobReverted state, got %d", reverted.State)
	}
	if reverted.Commitment != nil {
		t.Error("commitment should be nil after revert")
	}
}

func TestOutputHash_Deterministic(t *testing.T) {
	h1 := jobs.ComputeOutputHash([]byte("the answer"), "job-42", "bls-pub-key")
	h2 := jobs.ComputeOutputHash([]byte("the answer"), "job-42", "bls-pub-key")
	if string(h1) != string(h2) {
		t.Error("output hash not deterministic")
	}
}

func TestOutputHash_DifferentOutputs_DifferentHashes(t *testing.T) {
	h1 := jobs.ComputeOutputHash([]byte("answer A"), "job-1", "pub")
	h2 := jobs.ComputeOutputHash([]byte("answer B"), "job-1", "pub")
	if string(h1) == string(h2) {
		t.Error("different outputs produced same hash")
	}
}
