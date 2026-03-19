package registry_test

import (
	"os"
	"testing"

	"github.com/bigtchain/bigt/chain/registry"
	"github.com/bigtchain/bigt/chain/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, _ := os.MkdirTemp("", "bigt-reg-*")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProposeAndApproveModel(t *testing.T) {
	r := registry.New(newTestStore(t), 3)

	if err := r.ProposeModel(registry.MsgProposeModel{
		ModelID:      "llama3-70b",
		WeightsHash:  "0xdeadbeef",
		TokenizerHash: "0xcafebabe",
		Quantisation: "Q4_K_M",
	}); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// Before quorum, model not valid.
	ok, _ := r.IsValid("llama3-70b", 100)
	if ok {
		t.Error("model should not be valid before quorum")
	}

	// Two votes (quorum = 3, not yet reached).
	r.ApproveModel(registry.MsgApproveModel{ModelID: "llama3-70b", ValidatorAddr: "val1"}, 1) //nolint:errcheck
	r.ApproveModel(registry.MsgApproveModel{ModelID: "llama3-70b", ValidatorAddr: "val2"}, 2) //nolint:errcheck

	ok, _ = r.IsValid("llama3-70b", 100)
	if ok {
		t.Error("model should not be valid with 2/3 votes")
	}

	// Third vote reaches quorum.
	r.ApproveModel(registry.MsgApproveModel{ModelID: "llama3-70b", ValidatorAddr: "val3"}, 3) //nolint:errcheck

	ok, _ = r.IsValid("llama3-70b", 100)
	if !ok {
		t.Error("model should be valid after quorum")
	}
}

func TestDuplicateVote_Rejected(t *testing.T) {
	r := registry.New(newTestStore(t), 2)
	r.ProposeModel(registry.MsgProposeModel{ModelID: "gpt2", WeightsHash: "0x1234", GraceSlots: 100}) //nolint:errcheck

	r.ApproveModel(registry.MsgApproveModel{ModelID: "gpt2", ValidatorAddr: "val1"}, 1) //nolint:errcheck
	err := r.ApproveModel(registry.MsgApproveModel{ModelID: "gpt2", ValidatorAddr: "val1"}, 2)
	if err == nil {
		t.Fatal("expected error for duplicate vote")
	}
}

func TestDeprecateModel_GracePeriod(t *testing.T) {
	r := registry.New(newTestStore(t), 1)
	r.ProposeModel(registry.MsgProposeModel{ModelID: "m1", WeightsHash: "h1", GraceSlots: 100}) //nolint:errcheck
	r.ApproveModel(registry.MsgApproveModel{ModelID: "m1", ValidatorAddr: "val1"}, 1)           //nolint:errcheck

	// Deprecate at slot 1000.
	if err := r.DeprecateModel("m1", 1000); err != nil {
		t.Fatalf("deprecate: %v", err)
	}

	// Within grace (slot 1050): still valid.
	ok, _ := r.IsValid("m1", 1050)
	if !ok {
		t.Error("model should be valid within grace period")
	}

	// After grace (slot 1101): invalid.
	ok, _ = r.IsValid("m1", 1101)
	if ok {
		t.Error("model should be invalid after grace period ends")
	}
}

func TestUnknownModel_Invalid(t *testing.T) {
	r := registry.New(newTestStore(t), 1)
	ok, err := r.IsValid("unknown-model", 1)
	if err != nil {
		t.Fatalf("is valid: %v", err)
	}
	if ok {
		t.Error("unknown model should not be valid")
	}
}
