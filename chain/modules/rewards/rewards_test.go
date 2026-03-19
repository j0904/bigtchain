package rewards_test

import (
	"os"
	"testing"

	"github.com/bigtchain/bigt/chain/modules/rewards"
	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "bigt-rewards-*")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDistributeEpoch_SingleValidator(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	// Register one validator.
	st.RegisterValidator(staking.MsgRegisterValidator{ //nolint:errcheck
		Address: "val1", Bond: types.MinStake, Commission: 1000, // 10%
	}, 1e14)

	// Record jobs served.
	rm.RecordJobServed("val1", 0) //nolint:errcheck
	rm.RecordJobServed("val1", 0) //nolint:errcheck
	rm.RecordJobServed("val1", 0) //nolint:errcheck

	// Distribute epoch 0.
	const totalSupply = int64(100_000_000_000_000)
	epochRewards, err := rm.DistributeEpoch(0, totalSupply, 3)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if len(epochRewards) == 0 {
		t.Fatal("expected at least one reward entry")
	}
	r := epochRewards[0]
	if r.ValidatorAddr != "val1" {
		t.Errorf("wrong validator addr: %s", r.ValidatorAddr)
	}
	if r.ValidatorReward <= 0 {
		t.Errorf("validator reward should be positive, got %d", r.ValidatorReward)
	}
	if r.DelegatorReward <= 0 {
		t.Errorf("delegator reward should be positive, got %d", r.DelegatorReward)
	}
}

func TestDistributeEpoch_SlashedReducesReward(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	st.RegisterValidator(staking.MsgRegisterValidator{Address: "v1", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck
	st.RegisterValidator(staking.MsgRegisterValidator{Address: "v2", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck

	// v1: 3 jobs served, 1 slashed → 2 effective.
	rm.RecordJobServed("v1", 1) //nolint:errcheck
	rm.RecordJobServed("v1", 1) //nolint:errcheck
	rm.RecordJobServed("v1", 1) //nolint:errcheck
	rm.RecordJobSlashed("v1", 1) //nolint:errcheck

	// v2: 2 jobs served, 0 slashed → 2 effective.
	rm.RecordJobServed("v2", 1) //nolint:errcheck
	rm.RecordJobServed("v2", 1) //nolint:errcheck

	epochRewards, err := rm.DistributeEpoch(1, 1e14, 5)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if len(epochRewards) != 2 {
		t.Fatalf("expected 2 reward entries, got %d", len(epochRewards))
	}

	// Both have 2 effective jobs → equal rewards.
	var v1r, v2r rewards.EpochReward
	for _, r := range epochRewards {
		if r.ValidatorAddr == "v1" {
			v1r = r
		} else {
			v2r = r
		}
	}
	if v1r.ValidatorReward != v2r.ValidatorReward {
		t.Errorf("v1 reward %d != v2 reward %d (both have 2 effective jobs)", v1r.ValidatorReward, v2r.ValidatorReward)
	}
}

func TestDistributeEpoch_NoActivity_ReturnsNil(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	st.RegisterValidator(staking.MsgRegisterValidator{Address: "v", Bond: types.MinStake}, 1e14) //nolint:errcheck

	epochRewards, err := rm.DistributeEpoch(0, 1e14, 0)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if len(epochRewards) != 0 {
		t.Errorf("expected empty rewards for zero-activity epoch, got %d entries", len(epochRewards))
	}
}
