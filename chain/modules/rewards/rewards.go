// Package rewards implements per-epoch BIGT reward distribution.
package rewards

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/store"
)

// EpochStats tracks per-validator performance within an epoch.
type EpochStats struct {
	ValidatorAddr string `json:"validator_addr"`
	JobsServed    int64  `json:"jobs_served"`   // correctly committed
	JobsSlashed   int64  `json:"jobs_slashed"`  // slashed during epoch
	Epoch         int64  `json:"epoch"`
}

// EpochReward contains computed reward information for a single validator.
type EpochReward struct {
	ValidatorAddr    string `json:"validator_addr"`
	ValidatorReward  int64  `json:"validator_reward"` // uBIGT to validator
	DelegatorReward  int64  `json:"delegator_reward"` // uBIGT split among delegators
}

// InflationRateBPS is the annual inflation rate in basis points (500 = 5%).
const InflationRateBPS = int64(500)

// JobFeeUBIGT is a fixed fee per job paid by users (in uBIGT).
const JobFeeUBIGT = int64(1_000_000) // 1 BIGT

var (
	statsPrefix = []byte("epoch_stats/")
)

func statsKey(epoch int64, addr string) []byte {
	return []byte(fmt.Sprintf("epoch_stats/%020d/%s", epoch, addr))
}

// Module manages epoch reward accounting.
type Module struct {
	store   *store.Store
	staking *staking.Module
}

// New creates a new rewards module.
func New(s *store.Store, st *staking.Module) *Module {
	return &Module{store: s, staking: st}
}

// RecordJobServed records that a validator correctly served a job in the given epoch.
func (m *Module) RecordJobServed(validatorAddr string, epoch int64) error {
	stats, err := m.getStats(validatorAddr, epoch)
	if err != nil {
		return err
	}
	stats.JobsServed++
	return m.putStats(stats)
}

// RecordJobSlashed records a slash event for a validator in the given epoch.
func (m *Module) RecordJobSlashed(validatorAddr string, epoch int64) error {
	stats, err := m.getStats(validatorAddr, epoch)
	if err != nil {
		return err
	}
	stats.JobsSlashed++
	return m.putStats(stats)
}

// DistributeEpoch computes and returns rewards for all validators in the given epoch.
// totalSupply is used to compute the inflation component of the epoch reward pool.
// jobCount is the total number of jobs served in the epoch (for pool sizing).
func (m *Module) DistributeEpoch(epoch int64, totalSupply, jobCount int64) ([]EpochReward, error) {
	// Reward pool = inflation + job fees.
	// Annual inflation = totalSupply * InflationRateBPS / 10000
	// Per-epoch inflation = annual / 365 (approximate; 365 epochs per year when epochs = 24h)
	annualInflation := totalSupply * InflationRateBPS / 10_000
	epochInflation := annualInflation / 365
	jobFeePool := jobCount * JobFeeUBIGT
	pool := epochInflation + jobFeePool

	validators, err := m.staking.ListAll()
	if err != nil {
		return nil, err
	}

	// Compute total jobs served across all validators.
	var totalJobsServed int64
	for _, v := range validators {
		s, err := m.getStats(v.Address, epoch)
		if err != nil {
			return nil, err
		}
		// Reduce credit proportionally for slashed jobs.
		effective := s.JobsServed - s.JobsSlashed
		if effective < 0 {
			effective = 0
		}
		totalJobsServed += effective
	}
	if totalJobsServed == 0 {
		return nil, nil // No activity; no rewards.
	}

	var rewards []EpochReward
	for _, v := range validators {
		s, err := m.getStats(v.Address, epoch)
		if err != nil {
			return nil, err
		}
		effective := s.JobsServed - s.JobsSlashed
		if effective <= 0 {
			continue
		}
		// Validator share = pool * 80% * (effective / totalJobsServed)
		validatorPool := pool * 80 / 100
		delegatorPool := pool * 20 / 100

		valShare := validatorPool * effective / totalJobsServed
		delShare := delegatorPool * effective / totalJobsServed

		// Apply commission: validator keeps commission% of del share.
		valCommission := delShare * v.Commission / 10_000
		valShare += valCommission
		delShare -= valCommission

		rewards = append(rewards, EpochReward{
			ValidatorAddr:   v.Address,
			ValidatorReward: valShare,
			DelegatorReward: delShare,
		})
	}
	return rewards, nil
}

func (m *Module) getStats(addr string, epoch int64) (*EpochStats, error) {
	data, err := m.store.Get(statsKey(epoch, addr))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &EpochStats{ValidatorAddr: addr, Epoch: epoch}, nil
	}
	var s EpochStats
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Module) putStats(s *EpochStats) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return m.store.Set(statsKey(s.Epoch, s.ValidatorAddr), data)
}
