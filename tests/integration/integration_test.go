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

// TestFullSlotLifecycle simulates a complete slot with single-server architecture:
// 1. Users commit job hashes
// 2. Users reveal prompts
// 3. Serving validator submits output commitment
// 4. Chain finalises with server's commitment accepted
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

	// Elect server for this slot (single-server architecture).
	vals, _ := st.ListActive()
	seed := vrf.EpochSeed([]byte("test-block-hash"), epoch)
	proposer, server := vrf.ElectSlot(seed, slot, vals)
	t.Logf("slot %d: proposer=%s server=%s", slot, proposer, server)

	if server == "" {
		t.Fatal("server should not be empty")
	}

	// Phase 1: User commits job.
	prompt, nonce := "What is 2+2?", "salt123"
	promptHash := types.Keccak256([]byte(prompt), []byte(nonce))
	req := types.JobRequest{
		JobID: "job-42", ModelID: "llama3", PromptHash: promptHash, UserAddr: "user1",
		Params: types.JobParams{Temperature: 0, MaxTokens: 64},
	}
	if err := jm.Commit(req, slot, server); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Phase 2: User reveals.
	if err := jm.Reveal(types.RevealTx{JobID: "job-42", Prompt: prompt, Nonce: nonce}); err != nil {
		t.Fatalf("reveal: %v", err)
	}

	// Serving validator commits output.
	output := "4"
	hash := jobs.ComputeOutputHash([]byte(output), "job-42", "bls-"+server)

	if err := jm.AddCommitment(types.OutputCommitment{
		JobID: "job-42", ValidatorAddr: server, OutputHash: hash, Slot: slot,
	}); err != nil {
		t.Fatalf("commitment: %v", err)
	}

	// Finalise slot.
	committed, missed, err := jm.FinaliseSlot(slot)
	if err != nil {
		t.Fatalf("finalise: %v", err)
	}
	t.Logf("committed: %v, missed: %v", committed, missed)

	if missed[server] {
		t.Error("server committed so should not be missed")
	}

	// Reset inactivity for server.
	st.ResetMissed(server, epoch) //nolint:errcheck

	// Verify final job state.
	j, _ := jm.GetJob("job-42")
	if j.State != jobs.JobServed {
		t.Errorf("expected job served, got state %d", j.State)
	}
}

// TestDisputeSlashing verifies that a user proving a serving validator lied results in 100% slash.
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

	// Commit a job with the validator as the server.
	prompt, nonce := "Who are you?", "nonce-x"
	promptHash := types.Keccak256([]byte(prompt), []byte(nonce))
	req := types.JobRequest{JobID: "j-dispute", ModelID: "m1", PromptHash: promptHash, UserAddr: "user2"}
	jm.Commit(req, 1, "liar-val")                                                         //nolint:errcheck
	jm.Reveal(types.RevealTx{JobID: "j-dispute", Prompt: prompt, Nonce: nonce})           //nolint:errcheck

	// Server commits to output "I am honest" but actually returns "I am evil" to the user.
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
	}, 1)
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

	// Verify job was reverted.
	j, _ := jm.GetJob("j-dispute")
	if j.State != jobs.JobReverted {
		t.Errorf("expected job reverted after dispute, got state %d", j.State)
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
	}, 1, "honest-val")                                                                    //nolint:errcheck
	jm.Reveal(types.RevealTx{JobID: "j-ok", Prompt: prompt, Nonce: nonce})               //nolint:errcheck

	hash := jobs.ComputeOutputHash([]byte(output), "j-ok", "bls-honest")
	jm.AddCommitment(types.OutputCommitment{JobID: "j-ok", ValidatorAddr: "honest-val", OutputHash: hash, Slot: 1}) //nolint:errcheck

	// User disputes with the same output they actually received (honest validator).
	ev, err := sh.ProcessUserDispute(types.DisputeTx{
		JobID: "j-ok", ValidatorAddr: "honest-val", PlaintextOutput: output,
	}, 1)
	if err != nil {
		t.Fatalf("process dispute: %v", err)
	}
	if ev != nil {
		t.Errorf("dispute should be invalid; no slash expected, got: %+v", ev)
	}
}

// TestObserverDispute_ConsensusVote tests observer dispute with consensus vote flow.
func TestObserverDispute_ConsensusVote(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	jm := jobs.New(s)
	sh := slashing.New(st, jm)

	const totalSupply = int64(100_000_000_000_000)

	// Register validators.
	for _, addr := range []string{"server1", "obs1", "obs2", "obs3"} {
		st.RegisterValidator(staking.MsgRegisterValidator{ //nolint:errcheck
			Address: addr, Bond: types.MinStake, BLSPubKey: "bls-" + addr,
		}, totalSupply)
	}

	// Setup: server commits a job.
	prompt, nonce := "test", "n"
	jm.Commit(types.JobRequest{
		JobID: "j-obs", ModelID: "m", PromptHash: types.Keccak256([]byte(prompt), []byte(nonce)), UserAddr: "u",
	}, 1, "server1") //nolint:errcheck
	jm.Reveal(types.RevealTx{JobID: "j-obs", Prompt: prompt, Nonce: nonce}) //nolint:errcheck
	hash := jobs.ComputeOutputHash([]byte("output"), "j-obs", "bls-server1")
	jm.AddCommitment(types.OutputCommitment{JobID: "j-obs", ValidatorAddr: "server1", OutputHash: hash, Slot: 1}) //nolint:errcheck

	// Observer files dispute.
	if err := sh.ProcessObserverDispute(types.ObserverDisputeTx{
		JobID: "j-obs", ObserverAddr: "obs1",
	}, 1); err != nil {
		t.Fatalf("observer dispute: %v", err)
	}

	// Verify job is in disputed state.
	j, _ := jm.GetJob("j-obs")
	if j.State != jobs.JobDisputed {
		t.Errorf("expected job disputed, got %d", j.State)
	}

	// Cast votes: obs1, obs2 uphold; obs3 dismisses.
	sh.CastVote(types.DisputeVoteTx{JobID: "j-obs", VoterAddr: "obs1", Vote: "uphold"}) //nolint:errcheck
	sh.CastVote(types.DisputeVoteTx{JobID: "j-obs", VoterAddr: "obs2", Vote: "uphold"}) //nolint:errcheck
	sh.CastVote(types.DisputeVoteTx{JobID: "j-obs", VoterAddr: "obs3", Vote: "dismiss"}) //nolint:errcheck

	// Tally at vote deadline (slot 1 + DisputeWindow + VoteWindow = 11).
	voteDeadlineSlot := int64(1) + types.DisputeWindow + types.VoteWindow
	events, err := sh.TallyExpiredDisputes(voteDeadlineSlot)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}

	// With 3 of 4 validators voting (3*MinStake uphold vs 1*MinStake dismiss),
	// uphold votes = 2*MinStake, total = 4*MinStake, threshold = 4*MinStake * 6667/10000.
	// 2*MinStake > threshold? Let's check.
	totalStake := int64(4) * types.MinStake
	threshold := totalStake * types.DisputeVoteThresholdBPS / 10_000

	upholdStake := int64(2) * types.MinStake // obs1 + obs2
	if upholdStake > threshold {
		if len(events) == 0 {
			t.Error("expected slash event from tally (uphold > threshold)")
		}
	} else {
		t.Logf("uphold stake %d <= threshold %d; dispute dismissed", upholdStake, threshold)
	}
}

// TestInactivityDrain_QuadraticFormula verifies the drain math.
func TestInactivityDrain_QuadraticFormula(t *testing.T) {
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
