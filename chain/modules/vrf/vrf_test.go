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

func TestElectSlot_ReturnsProposerAndServer(t *testing.T) {
	seed := []byte("test-epoch-seed-000")
	vals := makeValidators("val1", "val2", "val3", "val4", "val5")

	proposer, server := vrf.ElectSlot(seed, 1, vals)

	if proposer == "" {
		t.Fatal("proposer should not be empty")
	}
	if server == "" {
		t.Fatal("server should not be empty")
	}
	// Proposer must not be the server (with enough validators).
	if proposer == server {
		t.Errorf("proposer %s should not be the same as server %s", proposer, server)
	}
}

func TestElectSlot_Deterministic(t *testing.T) {
	seed := []byte("deterministic-seed")
	vals := makeValidators("a", "b", "c", "d", "e")

	p1, s1 := vrf.ElectSlot(seed, 42, vals)
	p2, s2 := vrf.ElectSlot(seed, 42, vals)

	if p1 != p2 {
		t.Errorf("proposer not deterministic: %s != %s", p1, p2)
	}
	if s1 != s2 {
		t.Errorf("server not deterministic: %s != %s", s1, s2)
	}
}

func TestElectSlot_DifferentSlotsGiveDifferentElections(t *testing.T) {
	seed := []byte("epoch-seed-abc")
	vals := makeValidators("v1", "v2", "v3", "v4", "v5")

	p1, _ := vrf.ElectSlot(seed, 1, vals)
	p2, _ := vrf.ElectSlot(seed, 2, vals)
	// Very unlikely to be the same (1/5 chance per validator set of 5).
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

func TestElectSlot_SingleValidator(t *testing.T) {
	seed := []byte("seed")
	vals := makeValidators("v1")
	proposer, server := vrf.ElectSlot(seed, 1, vals)
	if proposer != "v1" {
		t.Errorf("expected v1 as proposer, got %s", proposer)
	}
	// With only 1 validator, proposer and server are the same.
	if server != "v1" {
		t.Errorf("expected v1 as server with 1 validator, got %s", server)
	}
}

func TestElectSlot_TwoValidators(t *testing.T) {
	seed := []byte("seed")
	vals := makeValidators("v1", "v2")
	proposer, server := vrf.ElectSlot(seed, 1, vals)
	if proposer == "" || server == "" {
		t.Fatal("proposer and server should both be assigned")
	}
	if proposer == server {
		t.Error("proposer and server should differ with 2 validators")
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
	proposer, server := vrf.ElectSlot(seed, 1, vals)
	for _, addr := range []string{proposer, server} {
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
