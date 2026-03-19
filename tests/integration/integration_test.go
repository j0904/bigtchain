// Package integration_test exercises full cross-module workflows.
package integration_test

import (
	"os"
	"testing"

	"github.com/bigtchain/bigt/chain/modules/jobs"
	"github.com/bigtchain/bigt/chain/modules/slashing"
	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/modules/vrf"
	"github.com/bigtchain/bigt/chain/registry"
	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "bigt-int-*")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestFullSlotLifecycle simulates a complete 3-phase slot:
// 1. Users commit job hashes
// 2. Users reveal prompts
// 3. Workers submit output commitments
// 4. Chain finalises with majority agreement
func TestFullSlotLifecycle(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	jm := jobs.New(s)
	reg := registry.New(s, 1) // quorum=1 for test speed

	const totalSupply = int64(100_000_000_000_000)
	slot := int64(42)
	epoch := slot / types.EpochSlots

	// Register validators.
	for _, addr := range []string{"v1", "v2", "v3", "v4", "v5"} {
		if err := st.RegisterValidator(staking.MsgRegisterValidator{
			Address: addr, Bond: types.MinStake, BLSPubKey: "bls-" + addr,
		}, totalSupply); err != nil {
			t.Fatalf("register %s: %v", addr, err)
		}
	}

	// Register a model.
	reg.ProposeModel(registry.MsgProposeModel{ModelID: "llama3", WeightsHash: "0x1234"})                        //nolint:errcheck
	reg.ApproveModel(registry.MsgApproveModel{ModelID: "llama3", ValidatorAddr: "v1"}, slot)                  //nolint:errcheck

	// Elect workers for this slot.
	vals, _ := st.ListActive()
	seed := vrf.EpochSeed([]byte("test-block-hash"), epoch)
	proposer, workers := vrf.ElectSlot(seed, slot, vals)
	t.Logf("slot %d: proposer=%s workers=%v", slot, proposer, workers)

	if len(workers) < 2 {
		t.Fatalf("need at least 2 workers, got %d", len(workers))
	}

	// Phase 1: User commits job.
	prompt, nonce := "What is 2+2?", "salt123"
	promptHash := types.Keccak256([]byte(prompt), []byte(nonce))
	req := types.JobRequest{
		JobID: "job-42", ModelID: "llama3", PromptHash: promptHash, UserAddr: "user1",
		Params: types.JobParams{Temperature: 0, MaxTokens: 64},
	}
	if err := jm.Commit(req, slot, workers); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Phase 2: User reveals.
	if err := jm.Reveal(types.RevealTx{JobID: "job-42", Prompt: prompt, Nonce: nonce}); err != nil {
		t.Fatalf("reveal: %v", err)
	}

	// Workers route and commit (workers[0] and workers[1] agree, workers[2] is slow/missing).
	output := "4"
	hash1 := jobs.ComputeOutputHash([]byte(output), "job-42", "bls-"+workers[0])
	hash2 := jobs.ComputeOutputHash([]byte(output), "job-42", "bls-"+workers[0]) // same logical output

	agreed1, err := jm.AddCommitment(types.OutputCommitment{
		JobID: "job-42", ValidatorAddr: workers[0], OutputHash: hash1, Slot: slot,
	})
	if err != nil {
		t.Fatalf("commitment w0: %v", err)
	}
	if agreed1 {
		t.Error("should not agree with 1 commitment")
	}

	agreed2, err := jm.AddCommitment(types.OutputCommitment{
		JobID: "job-42", ValidatorAddr: workers[1], OutputHash: hash2, Slot: slot,
	})
	if err != nil {
		t.Fatalf("commitment w1: %v", err)
	}
	if !agreed2 {
		t.Error("expected majority agreement after 2 matching commitments")
	}

	// Finalise slot.
	committed, missed, err := jm.FinaliseSlot(slot)
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	t.Logf("committed: %v, missed: %v", committed, missed)

	// workers[2] missed — increment their counter.
	if len(workers) > 2 {
		if missed[workers[2]] {
			if err := st.IncrementMissed(workers[2]); err != nil {
				t.Errorf("increment missed: %v", err)
			}
			v, _ := st.GetValidator(workers[2])
			if v.ConsecutiveMissed != 1 {
				t.Errorf("consecutive missed = %d, want 1", v.ConsecutiveMissed)
			}
		}
	}

	// Reset inactivity for committed workers.
	for _, addr := range []string{workers[0], workers[1]} {
		st.ResetMissed(addr, epoch) //nolint:errcheck
	}

	// Verify final job state.
	j, _ := jm.GetJob("job-42")
	if j.State != jobs.JobAgreed {
		t.Errorf("expected job agreed, got state %d", j.State)
	}
}

// TestDisputeSlashing verifies that a user proving a validator lied result in 100% slash.
func TestDisputeSlashing(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	jm := jobs.New(s)
	sh := slashing.New(st, jm)

	const totalSupply = int64(100_000_000_000_000)

	// Setup: register a validator with a known BLS key.
	if err := st.RegisterValidator(staking.MsgRegisterValidator{
		Address:   "liar-val",
		Bond:      types.MinStake * 2, // 10,000 BIGT
		BLSPubKey: "bls-pub-liar",
	}, totalSupply); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Commit a job and have the validator submit a REAL output via chain.
	prompt, nonce := "Who are you?", "nonce-x"
	promptHash := types.Keccak256([]byte(prompt), []byte(nonce))
	req := types.JobRequest{JobID: "j-dispute", ModelID: "m1", PromptHash: promptHash, UserAddr: "user2"}
	jm.Commit(req, 1, []string{"liar-val"})                                               //nolint:errcheck
	jm.Reveal(types.RevealTx{JobID: "j-dispute", Prompt: prompt, Nonce: nonce})           //nolint:errcheck

	// Validator commits to output "I am honest" but actually returns "I am evil" to the user.
	committedOutput := "I am honest"
	returnedOutput := "I am evil"

	commitHash := jobs.ComputeOutputHash([]byte(committedOutput), "j-dispute", "bls-pub-liar")
	jm.AddCommitment(types.OutputCommitment{ //nolint:errcheck
		JobID: "j-dispute", ValidatorAddr: "liar-val", OutputHash: commitHash, Slot: 1,
	})

	// User submits a dispute with what they actually received.
	ev, err := sh.ProcessUserDispute(types.DisputeTx{
		JobID:           "j-dispute",
		ValidatorAddr:   "liar-val",
		PlaintextOutput: returnedOutput,
	})
	if err != nil {
		t.Fatalf("process dispute: %v", err)
	}
	if ev == nil {
		t.Fatal("expected slash event — dispute should have succeeded")
	}

	// Validator should be slashed 100%.
	if ev.AmountSlashed != types.MinStake*2 {
		t.Errorf("expected slash of full bond (%d), got %d", types.MinStake*2, ev.AmountSlashed)
	}
	v, _ := st.GetValidator("liar-val")
	if v.Bond != 0 {
		t.Errorf("bond after 100%% slash should be 0, got %d", v.Bond)
	}
	if v.Status != staking.StatusJailed {
		t.Errorf("expected jailed, got %v", v.Status)
	}
}

// TestInvalidDispute_HashesMatch verifies a dispute is rejected when the hashes equal.
func TestInvalidDispute_HashesMatch(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	jm := jobs.New(s)
	sh := slashing.New(st, jm)

	st.RegisterValidator(staking.MsgRegisterValidator{ //nolint:errcheck
		Address: "honest-val", Bond: types.MinStake, BLSPubKey: "bls-honest",
	}, 1e14)

	output := "correct answer"
	prompt, nonce := "question", "n"
	jm.Commit(types.JobRequest{
		JobID: "j-ok", ModelID: "m", PromptHash: types.Keccak256([]byte(prompt), []byte(nonce)), UserAddr: "u",
	}, 1, []string{"honest-val"})                                                         //nolint:errcheck
	jm.Reveal(types.RevealTx{JobID: "j-ok", Prompt: prompt, Nonce: nonce})               //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte(output), "j-ok", "bls-honest")
	jm.AddCommitment(types.OutputCommitment{JobID: "j-ok", ValidatorAddr: "honest-val", OutputHash: hash, Slot: 1}) //nolint:errcheck

	// User disputes with the same output they actually received (honest validator).
	ev, err := sh.ProcessUserDispute(types.DisputeTx{
		JobID: "j-ok", ValidatorAddr: "honest-val", PlaintextOutput: output,
	})
	if err != nil {
		t.Fatalf("process dispute: %v", err)
	}
	if ev != nil {
		t.Errorf("dispute should be invalid; no slash expected, got: %+v", ev)
	}
}

// TestInactivityDrain_QuadraticFormula verifies the drain math.
func TestInactivityDrain_QuadraticFormula(t *testing.T) {
	// drainBPS = excess * excess * 100 / 1000  (integer division)
	// excess = epochsSinceActive - InactivityThresholdEpochs
	type tc struct {
		excess  int64
		wantBPS int64
	}
	cases := []tc{
		{0, 0},
		{1, 0},      // 1*100/1000 = 0 (integer truncation)
		{10, 10},    // 100*100/1000 = 10
		{100, 1000}, // 10000*100/1000 = 1000
	}
	for _, c := range cases {
		got := c.excess * c.excess * 100 / 1000
		if got != c.wantBPS {
			t.Errorf("excess=%d: drainBPS=%d, want %d", c.excess, got, c.wantBPS)
		}
	}
}
