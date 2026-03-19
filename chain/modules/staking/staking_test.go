package staking_test

import (
	"os"
	"testing"

	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "bigt-test-*")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegisterValidator_Success(t *testing.T) {
	m := staking.New(newTestStore(t))
	const totalSupply = int64(100_000_000_000_000)

	err := m.RegisterValidator(staking.MsgRegisterValidator{
		Address:   "val1",
		Bond:      types.MinStake,
		Moniker:   "test-val",
		Commission: 500,
	}, totalSupply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, err := m.GetValidator("val1")
	if err != nil {
		t.Fatalf("get validator: %v", err)
	}
	if v == nil {
		t.Fatal("validator not found")
	}
	if v.Bond != types.MinStake {
		t.Errorf("bond = %d, want %d", v.Bond, types.MinStake)
	}
	if v.Status != staking.StatusActive {
		t.Errorf("status = %v, want active", v.Status)
	}
}

func TestRegisterValidator_BelowMinStake(t *testing.T) {
	m := staking.New(newTestStore(t))
	err := m.RegisterValidator(staking.MsgRegisterValidator{
		Address: "val2",
		Bond:    types.MinStake - 1,
	}, 1_000_000_000_000)
	if err == nil {
		t.Fatal("expected error for bond below min stake")
	}
}

func TestRegisterValidator_DuplicateAddress(t *testing.T) {
	m := staking.New(newTestStore(t))
	msg := staking.MsgRegisterValidator{
		Address: "val3",
		Bond:    types.MinStake,
	}
	if err := m.RegisterValidator(msg, 1e14); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := m.RegisterValidator(msg, 1e14); err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestSlashBond(t *testing.T) {
	m := staking.New(newTestStore(t))
	if err := m.RegisterValidator(staking.MsgRegisterValidator{
		Address: "slash-val",
		Bond:    10_000_000_000, // 10,000 BIGT
	}, 1e14); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Slash 10% (1000 bps).
	amount, err := m.SlashBond("slash-val", 1000)
	if err != nil {
		t.Fatalf("slash: %v", err)
	}
	expected := int64(1_000_000_000) // 10% of 10,000 BIGT
	if amount != expected {
		t.Errorf("slash amount = %d, want %d", amount, expected)
	}

	v, _ := m.GetValidator("slash-val")
	remaining := int64(9_000_000_000)
	if v.Bond != remaining {
		t.Errorf("bond after slash = %d, want %d", v.Bond, remaining)
	}
}

func TestSlashBond_100Percent_Jails(t *testing.T) {
	m := staking.New(newTestStore(t))
	if err := m.RegisterValidator(staking.MsgRegisterValidator{
		Address: "jailed-val",
		Bond:    types.MinStake, // exactly at min stake
	}, 1e14); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := m.SlashBond("jailed-val", types.SlashMalicious)
	if err != nil {
		t.Fatalf("slash: %v", err)
	}

	v, _ := m.GetValidator("jailed-val")
	if v.Status != staking.StatusJailed {
		t.Errorf("expected jailed, got %v", v.Status)
	}
	if v.Bond != 0 {
		t.Errorf("bond after 100%% slash should be 0, got %d", v.Bond)
	}
}

func TestUnjail_RequiresMinStake(t *testing.T) {
	m := staking.New(newTestStore(t))
	m.RegisterValidator(staking.MsgRegisterValidator{Address: "v", Bond: types.MinStake}, 1e14) //nolint:errcheck
	m.SlashBond("v", 10_000)                                                                     //nolint:errcheck

	// Bond is 0; unjail should fail.
	if err := m.Unjail("v"); err == nil {
		t.Fatal("expected error: bond too low to unjail")
	}
}

func TestInactivity_Tracking(t *testing.T) {
	m := staking.New(newTestStore(t))
	m.RegisterValidator(staking.MsgRegisterValidator{Address: "lazy", Bond: types.MinStake}, 1e14) //nolint:errcheck

	for i := 0; i < 5; i++ {
		if err := m.IncrementMissed("lazy"); err != nil {
			t.Fatalf("increment missed: %v", err)
		}
	}

	v, _ := m.GetValidator("lazy")
	if v.ConsecutiveMissed != 5 {
		t.Errorf("consecutive missed = %d, want 5", v.ConsecutiveMissed)
	}

	m.ResetMissed("lazy", 1) //nolint:errcheck
	v, _ = m.GetValidator("lazy")
	if v.ConsecutiveMissed != 0 {
		t.Errorf("after reset, consecutive missed = %d, want 0", v.ConsecutiveMissed)
	}
}

func TestDelegate_Undelegate(t *testing.T) {
	m := staking.New(newTestStore(t))
	m.RegisterValidator(staking.MsgRegisterValidator{Address: "dval", Bond: types.MinStake}, 1e14) //nolint:errcheck

	if err := m.Delegate(staking.MsgDelegate{
		DelegatorAddr: "del1",
		ValidatorAddr: "dval",
		Amount:        2_000_000_000,
	}, 0); err != nil {
		t.Fatalf("delegate: %v", err)
	}

	v, _ := m.GetValidator("dval")
	if v.TotalStake != types.MinStake+2_000_000_000 {
		t.Errorf("total stake = %d, want %d", v.TotalStake, types.MinStake+2_000_000_000)
	}

	if err := m.Undelegate(staking.MsgUndelegate{
		DelegatorAddr: "del1",
		ValidatorAddr: "dval",
		Amount:        2_000_000_000,
	}, 0); err != nil {
		t.Fatalf("undelegate: %v", err)
	}

	v, _ = m.GetValidator("dval")
	if v.TotalStake != types.MinStake {
		t.Errorf("total stake after undelegate = %d, want %d", v.TotalStake, types.MinStake)
	}
}
