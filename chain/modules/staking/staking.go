// Package staking manages validator registration, bonding, and the active validator set.
package staking

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

// ValidatorStatus represents the current status of a validator.
type ValidatorStatus int

const (
	StatusActive    ValidatorStatus = iota
	StatusJailed
	StatusUnbonding
)

func (s ValidatorStatus) String() string {
	switch s {
	case StatusActive:
		return "active"
	case StatusJailed:
		return "jailed"
	case StatusUnbonding:
		return "unbonding"
	default:
		return "unknown"
	}
}

// Validator represents a registered validator.
type Validator struct {
	Address        string          `json:"address"`
	ConsensusPubKey string         `json:"consensus_pub_key"` // ed25519, base64
	BLSPubKey      string          `json:"bls_pub_key"`       // hex BLS12-381
	Bond           int64           `json:"bond"`              // uBIGT owned by validator
	TotalStake     int64           `json:"total_stake"`       // bond + delegations
	Commission     int64           `json:"commission_bps"`    // basis points
	Status         ValidatorStatus `json:"status"`
	Moniker        string          `json:"moniker"`
	RouterEndpoint string          `json:"router_endpoint"` // HTTP(S) endpoint

	// Inactivity tracking
	ConsecutiveMissed   int64 `json:"consecutive_missed"`
	LastActiveEpoch     int64 `json:"last_active_epoch"`

	// Key-evolving signature: current forward-secret key epoch
	KESEpoch int64 `json:"kes_epoch"`
}

// MsgRegisterValidator is a transaction payload to register a new validator.
type MsgRegisterValidator struct {
	Address         string `json:"address"`
	ConsensusPubKey string `json:"consensus_pub_key"`
	BLSPubKey       string `json:"bls_pub_key"`
	Bond            int64  `json:"bond"`
	Commission      int64  `json:"commission_bps"`
	Moniker         string `json:"moniker"`
	RouterEndpoint  string `json:"router_endpoint"`
}

// MsgDelegate is a transaction to delegate stake to a validator.
type MsgDelegate struct {
	DelegatorAddr string `json:"delegator_addr"`
	ValidatorAddr string `json:"validator_addr"`
	Amount        int64  `json:"amount"`
}

// MsgUndelegate queues a delegation withdrawal (21-day unbonding).
type MsgUndelegate struct {
	DelegatorAddr string `json:"delegator_addr"`
	ValidatorAddr string `json:"validator_addr"`
	Amount        int64  `json:"amount"`
}

// MsgUnjail re-activates a jailed validator (bond must be >= MinStake).
type MsgUnjail struct {
	ValidatorAddr string `json:"validator_addr"`
}

// UnbondingEntry records a pending unbonding.
type UnbondingEntry struct {
	DelegatorAddr string `json:"delegator_addr"`
	ValidatorAddr string `json:"validator_addr"`
	Amount        int64  `json:"amount"`
	MatureSlot    int64  `json:"mature_slot"` // slot when unbonding is complete
}

var (
	keyValPrefix  = []byte("val/")
	keyUBPrefix   = []byte("ub/")
	keyTotalStake = []byte("total_stake")
)

func valKey(addr string) []byte {
	return append(keyValPrefix, []byte(addr)...)
}

func ubKey(mature int64, delegator, validator string) []byte {
	return []byte(fmt.Sprintf("ub/%020d/%s/%s", mature, delegator, validator))
}

// Module holds staking state.
type Module struct {
	store *store.Store
}

// New creates a new staking module backed by the given store.
func New(s *store.Store) *Module {
	return &Module{store: s}
}

// RegisterValidator registers a new validator. Validates min stake and 5% cap.
// totalSupply is used to compute the cap (validator bond must not exceed 5% of total supply).
func (m *Module) RegisterValidator(msg MsgRegisterValidator, totalSupply int64) error {
	if msg.Bond < types.MinStake {
		return fmt.Errorf("bond %d below min stake %d", msg.Bond, types.MinStake)
	}
	// Check 5% cap against total supply (not current stake) so bootstrap works.
	if totalSupply > 0 && msg.Bond*100 > totalSupply*types.MaxValidatorPct {
		return fmt.Errorf("bond %d exceeds 5%% of total supply (%d)", msg.Bond, totalSupply)
	}

	existing, err := m.GetValidator(msg.Address)
	if err != nil {
		return err
	}
	if existing != nil {
		return fmt.Errorf("validator %s already registered", msg.Address)
	}

	v := &Validator{
		Address:         msg.Address,
		ConsensusPubKey: msg.ConsensusPubKey,
		BLSPubKey:       msg.BLSPubKey,
		Bond:            msg.Bond,
		TotalStake:      msg.Bond,
		Commission:      msg.Commission,
		Status:          StatusActive,
		Moniker:         msg.Moniker,
		RouterEndpoint:  msg.RouterEndpoint,
	}
	if err := m.putValidator(v); err != nil {
		return err
	}
	return m.addToTotalStake(msg.Bond)
}

// Delegate increases a validator's total stake by the delegation amount.
func (m *Module) Delegate(msg MsgDelegate, currentSlot int64) error {
	v, err := m.GetValidator(msg.ValidatorAddr)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("validator %s not found", msg.ValidatorAddr)
	}
	if v.Status == StatusJailed {
		return fmt.Errorf("cannot delegate to jailed validator %s", msg.ValidatorAddr)
	}
	v.TotalStake += msg.Amount
	if err := m.putValidator(v); err != nil {
		return err
	}
	return m.addToTotalStake(msg.Amount)
}

// Undelegate queues an unbonding entry (21-day period).
func (m *Module) Undelegate(msg MsgUndelegate, currentSlot int64) error {
	v, err := m.GetValidator(msg.ValidatorAddr)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("validator %s not found", msg.ValidatorAddr)
	}

	matureSlot := currentSlot + 21*24*int64(types.EpochSlots)/int64(types.EpochSlots)*int64(types.EpochSlots)
	// 21 days * 14400 slots/day
	matureSlot = currentSlot + 21*int64(types.EpochSlots)

	entry := &UnbondingEntry{
		DelegatorAddr: msg.DelegatorAddr,
		ValidatorAddr: msg.ValidatorAddr,
		Amount:        msg.Amount,
		MatureSlot:    matureSlot,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	v.TotalStake -= msg.Amount
	if err := m.putValidator(v); err != nil {
		return err
	}
	return m.store.Set(ubKey(matureSlot, msg.DelegatorAddr, msg.ValidatorAddr), data)
}

// ProcessMatureUnbondings releases all unbondings whose MatureSlot <= currentSlot.
// Returns the total amount released (to be credited to accounts off-chain or in a balance module).
func (m *Module) ProcessMatureUnbondings(currentSlot int64) (int64, error) {
	var released int64
	var toDelete [][]byte

	prefix := []byte("ub/")
	err := m.store.Scan(prefix, func(key, val []byte) bool {
		var entry UnbondingEntry
		if err := json.Unmarshal(val, &entry); err != nil {
			return false
		}
		if entry.MatureSlot <= currentSlot {
			released += entry.Amount
			toDelete = append(toDelete, append([]byte{}, key...))
		}
		return true
	})
	if err != nil {
		return 0, err
	}

	batch := m.store.Batch()
	for _, k := range toDelete {
		batch.Delete(k)
	}
	if err := batch.Flush(); err != nil {
		return 0, err
	}
	if released > 0 {
		if err := m.addToTotalStake(-released); err != nil {
			return 0, err
		}
	}
	return released, nil
}

// Unjail re-activates a jailed validator.
func (m *Module) Unjail(addr string) error {
	v, err := m.GetValidator(addr)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("validator %s not found", addr)
	}
	if v.Status != StatusJailed {
		return fmt.Errorf("validator %s is not jailed", addr)
	}
	if v.Bond < types.MinStake {
		return fmt.Errorf("bond %d < min stake %d, top up first", v.Bond, types.MinStake)
	}
	v.Status = StatusActive
	v.ConsecutiveMissed = 0
	return m.putValidator(v)
}

// SlashBond reduces a validator's bond by the given fraction (basis points).
// Returns the amount slashed.
func (m *Module) SlashBond(addr string, bps int64) (int64, error) {
	v, err := m.GetValidator(addr)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, fmt.Errorf("validator %s not found", addr)
	}
	amount := v.Bond * bps / 10_000
	if amount > v.Bond {
		amount = v.Bond
	}
	v.Bond -= amount
	v.TotalStake -= amount
	if v.Bond < types.MinStake {
		v.Status = StatusJailed
	}
	if err := m.putValidator(v); err != nil {
		return 0, err
	}
	if err := m.addToTotalStake(-amount); err != nil {
		return 0, err
	}
	return amount, nil
}

// IncrementMissed increments the consecutive missed counter for a validator.
func (m *Module) IncrementMissed(addr string) error {
	v, err := m.GetValidator(addr)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("validator %s not found", addr)
	}
	v.ConsecutiveMissed++
	return m.putValidator(v)
}

// ResetMissed resets the consecutive missed counter.
func (m *Module) ResetMissed(addr string, epoch int64) error {
	v, err := m.GetValidator(addr)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("validator %s not found", addr)
	}
	v.ConsecutiveMissed = 0
	v.LastActiveEpoch = epoch
	return m.putValidator(v)
}

// GetValidator retrieves a validator by address. Returns (nil, nil) if not found.
func (m *Module) GetValidator(addr string) (*Validator, error) {
	data, err := m.store.Get(valKey(addr))
	if err != nil || data == nil {
		return nil, err
	}
	var v Validator
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ListActive returns all active validators.
func (m *Module) ListActive() ([]*Validator, error) {
	var result []*Validator
	err := m.store.Scan(keyValPrefix, func(_, val []byte) bool {
		var v Validator
		if err := json.Unmarshal(val, &v); err != nil {
			return false
		}
		if v.Status == StatusActive {
			result = append(result, &v)
		}
		return true
	})
	return result, err
}

// ListAll returns all validators regardless of status.
func (m *Module) ListAll() ([]*Validator, error) {
	var result []*Validator
	err := m.store.Scan(keyValPrefix, func(_, val []byte) bool {
		var v Validator
		if err := json.Unmarshal(val, &v); err != nil {
			return false
		}
		result = append(result, &v)
		return true
	})
	return result, err
}

// TotalStake returns the current total stake in the network.
func (m *Module) TotalStake() (int64, error) {
	data, err := m.store.Get(keyTotalStake)
	if err != nil || data == nil {
		return 0, err
	}
	var total int64
	if err := json.Unmarshal(data, &total); err != nil {
		return 0, err
	}
	return total, nil
}

func (m *Module) putValidator(v *Validator) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.store.Set(valKey(v.Address), data)
}

func (m *Module) addToTotalStake(delta int64) error {
	total, err := m.TotalStake()
	if err != nil {
		return err
	}
	total += delta
	data, err := json.Marshal(total)
	if err != nil {
		return err
	}
	return m.store.Set(keyTotalStake, data)
}
