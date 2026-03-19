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

func TestDistributeEpoch_SingleValidator_ServingAndObserving(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	// Register one validator.
	st.RegisterValidator(staking.MsgRegisterValidator{ //nolint:errcheck
		Address: "val1", Bond: types.MinStake, Commission: 1000, // 10%
	}, 1e14)

	// Record jobs served and observed.
	rm.RecordJobServed("val1", 0)   //nolint:errcheck
	rm.RecordJobServed("val1", 0)   //nolint:errcheck
	rm.RecordJobServed("val1", 0)   //nolint:errcheck
	rm.RecordJobObserved("val1", 0) //nolint:errcheck

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
	if r.ServingReward <= 0 {
		t.Errorf("serving reward should be positive, got %d", r.ServingReward)
	}
	if r.ObserverReward <= 0 {
		t.Errorf("observer reward should be positive, got %d", r.ObserverReward)
	}
	if r.DelegatorReward <= 0 {
		t.Errorf("delegator reward should be positive, got %d", r.DelegatorReward)
	}
}

func TestDistributeEpoch_RevenueSharing_50_30_15_5(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	// Register two validators with no commission (simplifies math).
	st.RegisterValidator(staking.MsgRegisterValidator{Address: "server", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck
	st.RegisterValidator(staking.MsgRegisterValidator{Address: "observer", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck

	// Server serves 3 jobs; observer observes 3 jobs.
	for i := 0; i < 3; i++ {
		rm.RecordJobServed("server", 1)   //nolint:errcheck
		rm.RecordJobObserved("observer", 1) //nolint:errcheck
	}

	epochRewards, err := rm.DistributeEpoch(1, 1e14, 3)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}

	var serverR, observerR rewards.EpochReward
	for _, r := range epochRewards {
		switch r.ValidatorAddr {
		case "server":
			serverR = r
		case "observer":
			observerR = r
		}
	}

	// Server should get 50% serving reward + 15% delegator reward.
	if serverR.ServingReward <= 0 {
		t.Errorf("server should have serving reward, got %d", serverR.ServingReward)
	}
	if serverR.DelegatorReward <= 0 {
		t.Errorf("server should have delegator reward, got %d", serverR.DelegatorReward)
	}

	// Observer should get 30% observer reward.
	if observerR.ObserverReward <= 0 {
		t.Errorf("observer should have observer reward, got %d", observerR.ObserverReward)
	}

	// Check approximate split ratios (serving > observer > delegator).
	totalReward := serverR.ServingReward + serverR.DelegatorReward + observerR.ObserverReward
	servingPct := serverR.ServingReward * 100 / totalReward
	observerPct := observerR.ObserverReward * 100 / totalReward

	// Serving should be roughly 50%, observer roughly 30%.
	if servingPct < 45 || servingPct > 55 {
		t.Errorf("serving percentage should be ~50%%, got %d%%", servingPct)
	}
	if observerPct < 25 || observerPct > 35 {
		t.Errorf("observer percentage should be ~30%%, got %d%%", observerPct)
	}
}

func TestDistributeEpoch_SlashedReducesReward(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	st.RegisterValidator(staking.MsgRegisterValidator{Address: "v1", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck
	st.RegisterValidator(staking.MsgRegisterValidator{Address: "v2", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck

	// v1: 3 served, 1 slashed → 2 effective.
	rm.RecordJobServed("v1", 1) //nolint:errcheck
	rm.RecordJobServed("v1", 1) //nolint:errcheck
	rm.RecordJobServed("v1", 1) //nolint:errcheck
	rm.RecordJobSlashed("v1", 1) //nolint:errcheck

	// v2: 2 served → 2 effective.
	rm.RecordJobServed("v2", 1) //nolint:errcheck
	rm.RecordJobServed("v2", 1) //nolint:errcheck

	epochRewards, err := rm.DistributeEpoch(1, 1e14, 5)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if len(epochRewards) != 2 {
		t.Fatalf("expected 2 reward entries, got %d", len(epochRewards))
	}

	var v1r, v2r rewards.EpochReward
	for _, r := range epochRewards {
		if r.ValidatorAddr == "v1" {
			v1r = r
		} else {
			v2r = r
		}
	}
	if v1r.ServingReward != v2r.ServingReward {
		t.Errorf("v1 serving reward %d != v2 serving reward %d (both have 2 effective)", v1r.ServingReward, v2r.ServingReward)
	}
}

func TestDistributeEpoch_BountyReward(t *testing.T) {
	s := newTestStore(t)
	st := staking.New(s)
	rm := rewards.New(s, st)

	st.RegisterValidator(staking.MsgRegisterValidator{Address: "v1", Bond: types.MinStake, Commission: 0}, 1e14) //nolint:errcheck

	rm.RecordJobServed("v1", 1)   //nolint:errcheck
	rm.RecordDisputeWon("v1", 1)  //nolint:errcheck

	epochRewards, err := rm.DistributeEpoch(1, 1e14, 1)
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if len(epochRewards) == 0 {
		t.Fatal("expected reward entry")
	}
	if epochRewards[0].BountyReward <= 0 {
		t.Errorf("expected bounty reward, got %d", epochRewards[0].BountyReward)
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
