// Package registry implements the on-chain ModelRegistry.
// Validators may only serve registered (governance-approved) model versions.
package registry

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/store"
)

// ModelStatus represents the lifecycle state of a model.
type ModelStatus int

const (
	ModelProposed  ModelStatus = iota // Pending governance vote
	ModelActive                       // Approved; validators must serve this
	ModelDeprecated                   // Deprecated; still valid during grace period
)

// Model represents an approved model version.
type Model struct {
	ModelID       string      `json:"model_id"`
	WeightsHash   string      `json:"weights_hash"`   // hex keccak256 of model weights
	TokenizerHash string      `json:"tokenizer_hash"` // hex keccak256 of tokenizer
	Quantisation  string      `json:"quantisation"`   // e.g. "Q4_K_M"
	Status        ModelStatus `json:"status"`
	ApprovedAtSlot int64      `json:"approved_at_slot"`
	DeprecatedAtSlot int64    `json:"deprecated_at_slot"` // 0 if not deprecated
	GraceSlots    int64       `json:"grace_slots"` // slots after deprecation still valid
	Votes         []string    `json:"votes"` // validator addresses that voted to approve
}

// MsgProposeModel is a governance tx to propose a new model.
type MsgProposeModel struct {
	ModelID       string `json:"model_id"`
	WeightsHash   string `json:"weights_hash"`
	TokenizerHash string `json:"tokenizer_hash"`
	Quantisation  string `json:"quantisation"`
	GraceSlots    int64  `json:"grace_slots"`
}

// MsgApproveModel is a governance vote tx submitted by a validator.
type MsgApproveModel struct {
	ModelID       string `json:"model_id"`
	ValidatorAddr string `json:"validator_addr"`
}

// GracePeriodSlots is the default number of slots a deprecated model remains valid.
// 14,400 slots ≈ 1 epoch (24h), giving validators time to update their backends.
const GracePeriodSlots = int64(14_400)

var (
	modelPrefix = []byte("model/")
)

func modelKey(id string) []byte {
	return append(modelPrefix, []byte(id)...)
}

// Registry manages the on-chain model registry.
type Registry struct {
	store          *store.Store
	quorumRequired int // number of validator votes to approve a model
}

// New creates a new Registry.
func New(s *store.Store, quorum int) *Registry {
	return &Registry{store: s, quorumRequired: quorum}
}

// ProposeModel adds a new model in Proposed state.
func (r *Registry) ProposeModel(msg MsgProposeModel) error {
	existing, err := r.GetModel(msg.ModelID)
	if err != nil {
		return err
	}
	if existing != nil && existing.Status != ModelDeprecated {
		return fmt.Errorf("model %s already exists with status %d", msg.ModelID, existing.Status)
	}
	grace := msg.GraceSlots
	if grace == 0 {
		grace = GracePeriodSlots
	}
	m := &Model{
		ModelID:       msg.ModelID,
		WeightsHash:   msg.WeightsHash,
		TokenizerHash: msg.TokenizerHash,
		Quantisation:  msg.Quantisation,
		Status:        ModelProposed,
		GraceSlots:    grace,
	}
	return r.putModel(m)
}

// ApproveModel records a validator vote. If quorum reached, model becomes Active.
func (r *Registry) ApproveModel(msg MsgApproveModel, currentSlot int64) error {
	m, err := r.GetModel(msg.ModelID)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("model %s not found", msg.ModelID)
	}
	if m.Status != ModelProposed {
		return fmt.Errorf("model %s is not in proposed state", msg.ModelID)
	}
	// Deduplicate votes
	for _, v := range m.Votes {
		if v == msg.ValidatorAddr {
			return fmt.Errorf("validator %s already voted for model %s", msg.ValidatorAddr, msg.ModelID)
		}
	}
	m.Votes = append(m.Votes, msg.ValidatorAddr)
	if len(m.Votes) >= r.quorumRequired {
		m.Status = ModelActive
		m.ApprovedAtSlot = currentSlot
	}
	return r.putModel(m)
}

// DeprecateModel marks a model as deprecated, starting the grace period.
func (r *Registry) DeprecateModel(modelID string, currentSlot int64) error {
	m, err := r.GetModel(modelID)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("model %s not found", modelID)
	}
	m.Status = ModelDeprecated
	m.DeprecatedAtSlot = currentSlot
	return r.putModel(m)
}

// IsValid returns true if a model is currently valid for serving
// (Active, or Deprecated but within grace period).
func (r *Registry) IsValid(modelID string, currentSlot int64) (bool, error) {
	m, err := r.GetModel(modelID)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	switch m.Status {
	case ModelActive:
		return true, nil
	case ModelDeprecated:
		return currentSlot <= m.DeprecatedAtSlot+m.GraceSlots, nil
	default:
		return false, nil
	}
}

// GetModel retrieves a model by ID. Returns (nil, nil) if not found.
func (r *Registry) GetModel(modelID string) (*Model, error) {
	data, err := r.store.Get(modelKey(modelID))
	if err != nil || data == nil {
		return nil, err
	}
	var m Model
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListActive returns all currently active models.
func (r *Registry) ListActive(currentSlot int64) ([]*Model, error) {
	var result []*Model
	err := r.store.Scan(modelPrefix, func(_, val []byte) bool {
		var m Model
		if err := json.Unmarshal(val, &m); err != nil {
			return false
		}
		ok := m.Status == ModelActive ||
			(m.Status == ModelDeprecated && currentSlot <= m.DeprecatedAtSlot+m.GraceSlots)
		if ok {
			result = append(result, &m)
		}
		return true
	})
	return result, err
}

func (r *Registry) putModel(m *Model) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return r.store.Set(modelKey(m.ModelID), data)
}
