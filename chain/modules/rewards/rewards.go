// Package rewards implements per-epoch BIGT reward distribution with
// the single-server revenue sharing model: 50% serving validator, 30% observer
// pool, 15% delegators, 5% dispute bounty reserve.
package rewards

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

// EpochStats tracks per-validator performance within an epoch.
type EpochStats struct {
	ValidatorAddr  string `json:"validator_addr"`
	JobsServed     int64  `json:"jobs_served"`     // served as elected server
	JobsObserved   int64  `json:"jobs_observed"`   // attested as observer
	JobsSlashed    int64  `json:"jobs_slashed"`    // slashed during epoch
	BlocksReverted int64  `json:"blocks_reverted"` // blocks reverted (dispute upheld)
	DisputesWon    int64  `json:"disputes_won"`    // successful disputes filed
	Epoch          int64  `json:"epoch"`
}

// EpochReward contains computed reward information for a single validator.
type EpochReward struct {
	ValidatorAddr   string `json:"validator_addr"`
	ServingReward   int64  `json:"serving_reward"`   // uBIGT for serving jobs
	ObserverReward  int64  `json:"observer_reward"`  // uBIGT from observer pool
	DelegatorReward int64  `json:"delegator_reward"` // uBIGT split among delegators
	BountyReward    int64  `json:"bounty_reward"`    // uBIGT from dispute bounties won
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

// RecordJobObserved records that a validator attested as an observer in the given epoch.
func (m *Module) RecordJobObserved(validatorAddr string, epoch int64) error {
	stats, err := m.getStats(validatorAddr, epoch)
	if err != nil {
		return err
	}
	stats.JobsObserved++
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

// RecordBlockReverted records a block reversion against a serving validator.
func (m *Module) RecordBlockReverted(validatorAddr string, epoch int64) error {
	stats, err := m.getStats(validatorAddr, epoch)
	if err != nil {
		return err
	}
	stats.BlocksReverted++
	return m.putStats(stats)
}

// RecordDisputeWon records a successful dispute filed by an observer.
func (m *Module) RecordDisputeWon(validatorAddr string, epoch int64) error {
	stats, err := m.getStats(validatorAddr, epoch)
	if err != nil {
		return err
	}
	stats.DisputesWon++
	return m.putStats(stats)
}

// DistributeEpoch computes and returns rewards for all validators in the given epoch.
// Revenue sharing: 50% serving, 30% observer pool, 15% delegators, 5% bounty reserve.
func (m *Module) DistributeEpoch(epoch int64, totalSupply, jobCount int64) ([]EpochReward, error) {
	// Reward pool = inflation + job fees.
	annualInflation := totalSupply * InflationRateBPS / 10_000
	epochInflation := annualInflation / 365
	jobFeePool := jobCount * JobFeeUBIGT
	pool := epochInflation + jobFeePool

	// Split pool using revenue sharing BPS constants.
	servingPool := pool * types.RewardServingBPS / 10_000
	observerPool := pool * types.RewardObserverBPS / 10_000
	delegatorPool := pool * types.RewardDelegatorBPS / 10_000
	bountyPool := pool * types.RewardBountyBPS / 10_000

	validators, err := m.staking.ListAll()
	if err != nil {
		return nil, err
	}

	// Compute totals for each pool's distribution.
	var totalEffectiveServed int64
	var totalObserved int64
	var totalDisputesWon int64
	for _, v := range validators {
		s, err := m.getStats(v.Address, epoch)
		if err != nil {
			return nil, err
		}
		effective := s.JobsServed - s.JobsSlashed - s.BlocksReverted
		if effective < 0 {
			effective = 0
		}
		totalEffectiveServed += effective
		totalObserved += s.JobsObserved
		totalDisputesWon += s.DisputesWon
	}

	if totalEffectiveServed == 0 && totalObserved == 0 {
		return nil, nil // No activity; no rewards.
	}

	var rewards []EpochReward
	for _, v := range validators {
		s, err := m.getStats(v.Address, epoch)
		if err != nil {
			return nil, err
		}

		var r EpochReward
		r.ValidatorAddr = v.Address

		// Serving reward: proportional to effective jobs served.
		effectiveServed := s.JobsServed - s.JobsSlashed - s.BlocksReverted
		if effectiveServed > 0 && totalEffectiveServed > 0 {
			r.ServingReward = servingPool * effectiveServed / totalEffectiveServed
		}

		// Observer reward: proportional to jobs observed.
		if s.JobsObserved > 0 && totalObserved > 0 {
			r.ObserverReward = observerPool * s.JobsObserved / totalObserved
		}

		// Delegator reward: proportional to total effective serving.
		if effectiveServed > 0 && totalEffectiveServed > 0 {
			delShare := delegatorPool * effectiveServed / totalEffectiveServed
			// Apply commission: validator keeps commission% of delegator share.
			valCommission := delShare * v.Commission / 10_000
			r.ServingReward += valCommission
			r.DelegatorReward = delShare - valCommission
		}

		// Bounty reward: proportional to successful disputes.
		if s.DisputesWon > 0 && totalDisputesWon > 0 {
			r.BountyReward = bountyPool * s.DisputesWon / totalDisputesWon
		}

		if r.ServingReward > 0 || r.ObserverReward > 0 || r.DelegatorReward > 0 || r.BountyReward > 0 {
			rewards = append(rewards, r)
		}
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
