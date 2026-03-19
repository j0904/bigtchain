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
	workers := []string{"w1", "w2", "w3"}

	// Phase 1: Commit
	req := makeJobReq("job-001", "llama3")
	if err := m.Commit(req, 1, workers); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Phase 2: Reveal
	rev := types.RevealTx{JobID: "job-001", Prompt: "hello", Nonce: "abc"}
	if err := m.Reveal(rev); err != nil {
		t.Fatalf("reveal: %v", err)
	}

	// Worker 1 commits
	hash := jobs.ComputeOutputHash([]byte("the answer is 42"), "job-001", "bls-pub-w1")
	agreed, err := m.AddCommitment(types.OutputCommitment{
		JobID: "job-001", ValidatorAddr: "w1", OutputHash: hash, Slot: 1,
	})
	if err != nil {
		t.Fatalf("add commitment w1: %v", err)
	}
	if agreed {
		t.Error("should not agree with 1 of 3")
	}

	// Worker 2 commits same hash -> majority reached.
	hash2 := jobs.ComputeOutputHash([]byte("the answer is 42"), "job-001", "bls-pub-w1")
	agreed, err = m.AddCommitment(types.OutputCommitment{
		JobID: "job-001", ValidatorAddr: "w2", OutputHash: hash2, Slot: 1,
	})
	if err != nil {
		t.Fatalf("add commitment w2: %v", err)
	}
	if !agreed {
		t.Error("expected majority agreement with 2 matching commitments")
	}

	j, _ := m.GetJob("job-001")
	if j.State != jobs.JobAgreed {
		t.Errorf("expected job state Agreed, got %d", j.State)
	}
}

func TestReveal_WrongNonce_Rejected(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-002", "llama3")
	m.Commit(req, 1, []string{"w1", "w2", "w3"}) //nolint:errcheck

	rev := types.RevealTx{JobID: "job-002", Prompt: "hello", Nonce: "wrong-nonce"}
	if err := m.Reveal(rev); err == nil {
		t.Fatal("expected reveal to fail with wrong nonce")
	}
}

func TestDuplicateCommitment_Rejected(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-003", "gpt2")
	m.Commit(req, 1, []string{"w1", "w2", "w3"}) //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-003", Prompt: "hello", Nonce: "abc"}) //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte("output"), "job-003", "pub")
	m.AddCommitment(types.OutputCommitment{JobID: "job-003", ValidatorAddr: "w1", OutputHash: hash, Slot: 1}) //nolint:errcheck

	// Second commitment from same validator -> should fail.
	_, err := m.AddCommitment(types.OutputCommitment{JobID: "job-003", ValidatorAddr: "w1", OutputHash: hash, Slot: 1})
	if err == nil {
		t.Fatal("expected error for duplicate commitment from same validator")
	}
}

func TestNonWorker_Commitment_Rejected(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-004", "llama3")
	m.Commit(req, 1, []string{"w1", "w2", "w3"})                                                     //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-004", Prompt: "hello", Nonce: "abc"})                        //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte("output"), "job-004", "pub")
	_, err := m.AddCommitment(types.OutputCommitment{JobID: "job-004", ValidatorAddr: "w99", OutputHash: hash, Slot: 1})
	if err == nil {
		t.Fatal("expected error: w99 is not an elected worker")
	}
}

func TestFinaliseSlot_MarksMissedWorkers(t *testing.T) {
	m := jobs.New(newTestStore(t))
	req := makeJobReq("job-005", "llama3")
	workers := []string{"w1", "w2", "w3"}
	m.Commit(req, 5, workers)                                                              //nolint:errcheck
	m.Reveal(types.RevealTx{JobID: "job-005", Prompt: "hello", Nonce: "abc"})             //nolint:errcheck

	// Only w1 commits.
	hash := jobs.ComputeOutputHash([]byte("output"), "job-005", "pub")
	m.AddCommitment(types.OutputCommitment{JobID: "job-005", ValidatorAddr: "w1", OutputHash: hash, Slot: 5}) //nolint:errcheck

	_, missed, err := m.FinaliseSlot(5)
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	if !missed["w2"] || !missed["w3"] {
		t.Errorf("expected w2 and w3 to be missed, got %v", missed)
	}
	if missed["w1"] {
		t.Error("w1 committed so should not be missed")
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
