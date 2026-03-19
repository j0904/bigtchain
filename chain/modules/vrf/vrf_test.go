package vrf_test

import (
	"encoding/binary"
	"testing"

	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/modules/vrf"
)

func makeValidators(addrs ...string) []*staking.Validator {
	vals := make([]*staking.Validator, len(addrs))
	for i, addr := range addrs {
		vals[i] = &staking.Validator{
			Address:    addr,
			TotalStake: 10_000_000_000,
			Status:     staking.StatusActive,
		}
	}
	return vals
}

func TestElectSlot_ReturnsProposerAndWorkers(t *testing.T) {
	seed := []byte("test-epoch-seed-000")
	vals := makeValidators("val1", "val2", "val3", "val4", "val5")

	proposer, workers := vrf.ElectSlot(seed, 1, vals)

	if proposer == "" {
		t.Fatal("proposer should not be empty")
	}
	if len(workers) != 3 {
		t.Errorf("expected 3 workers, got %d", len(workers))
	}
	// Proposer must not appear in workers.
	for _, w := range workers {
		if w == proposer {
			t.Errorf("proposer %s also appears in workers", proposer)
		}
	}
}

func TestElectSlot_Deterministic(t *testing.T) {
	seed := []byte("deterministic-seed")
	vals := makeValidators("a", "b", "c", "d", "e")

	p1, w1 := vrf.ElectSlot(seed, 42, vals)
	p2, w2 := vrf.ElectSlot(seed, 42, vals)

	if p1 != p2 {
		t.Errorf("proposer not deterministic: %s != %s", p1, p2)
	}
	for i := range w1 {
		if w1[i] != w2[i] {
			t.Errorf("worker[%d] not deterministic: %s != %s", i, w1[i], w2[i])
		}
	}
}

func TestElectSlot_DifferentSlotsGiveDifferentElections(t *testing.T) {
	seed := []byte("epoch-seed-abc")
	vals := makeValidators("v1", "v2", "v3", "v4", "v5")

	p1, _ := vrf.ElectSlot(seed, 1, vals)
	p2, _ := vrf.ElectSlot(seed, 2, vals)
	// Very unlikely to be the same (1/5 chance per validator set of 5).
	// We test over many slots that not all are the same proposer.
	sameCount := 0
	for slot := int64(1); slot <= 20; slot++ {
		px, _ := vrf.ElectSlot(seed, slot, vals)
		if px == p1 {
			sameCount++
		}
	}
	if sameCount == 20 {
		t.Error("all 20 slots elected the same proposer — VRF may be broken")
	}
	_ = p2
}

func TestElectSlot_TooFewValidators(t *testing.T) {
	seed := []byte("seed")
	// Only 2 validators; should return proposer + 1 worker (not 3).
	vals := makeValidators("v1", "v2")
	_, workers := vrf.ElectSlot(seed, 1, vals)
	if len(workers) != 1 {
		t.Errorf("expected 1 worker with 2 validators, got %d", len(workers))
	}
}

func TestElectSlot_JailedExcluded(t *testing.T) {
	seed := []byte("jail-test")
	vals := []*staking.Validator{
		{Address: "active1", TotalStake: 10_000_000_000, Status: staking.StatusActive},
		{Address: "jailed1", TotalStake: 10_000_000_000, Status: staking.StatusJailed},
		{Address: "active2", TotalStake: 10_000_000_000, Status: staking.StatusActive},
		{Address: "active3", TotalStake: 10_000_000_000, Status: staking.StatusActive},
		{Address: "active4", TotalStake: 10_000_000_000, Status: staking.StatusActive},
	}
	proposer, workers := vrf.ElectSlot(seed, 1, vals)
	all := append([]string{proposer}, workers...)
	for _, addr := range all {
		if addr == "jailed1" {
			t.Errorf("jailed validator was elected: %s", addr)
		}
	}
}

func TestEpochSeed_Deterministic(t *testing.T) {
	hash := make([]byte, 32)
	binary.BigEndian.PutUint64(hash, 12345)
	s1 := vrf.EpochSeed(hash, 1)
	s2 := vrf.EpochSeed(hash, 1)
	if string(s1) != string(s2) {
		t.Error("epoch seed not deterministic")
	}
}

func TestEpochSeed_DifferentEpochs(t *testing.T) {
	hash := make([]byte, 32)
	s1 := vrf.EpochSeed(hash, 1)
	s2 := vrf.EpochSeed(hash, 2)
	if string(s1) == string(s2) {
		t.Error("different epochs produced same seed")
	}
}
