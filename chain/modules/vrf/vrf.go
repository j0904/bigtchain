// Package vrf implements VRF-based slot election for proposer and a single serving validator.
// We use a deterministic pseudorandom sort as a placeholder; a production
// implementation should use ECVRF (IETF draft-irtf-cfrg-vrf-15) with the
// validator's ed25519 consensus key.
package vrf

import (
	"encoding/binary"
	"sort"

	"github.com/bigtchain/bigt/chain/modules/staking"
	"github.com/bigtchain/bigt/chain/types"
)

// Proof holds the VRF output for a validator+slot pair.
type Proof struct {
	ValidatorAddr string
	Hash          []byte // 32-byte pseudorandom output
	Stake         int64
}

// ElectSlot selects 1 proposer and 1 serving validator from the active
// validator set for the given slot. epochSeed is committed at epoch start.
//
// Selection: compute hash = keccak256(epochSeed || slotNumber || validatorAddr)
// for each active validator, sort ascending. Lowest hash = proposer; second
// lowest = serving validator. All other active validators are observers.
// Validators stake-weights the sort via a secondary sort on stake
// (higher stake breaks ties in favour of the validator).
func ElectSlot(epochSeed []byte, slot int64, validators []*staking.Validator) (proposer string, server string) {
	slotBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(slotBytes, uint64(slot))

	proofs := make([]Proof, 0, len(validators))
	for _, v := range validators {
		if v.Status != staking.StatusActive {
			continue
		}
		h := types.Keccak256(epochSeed, slotBytes, []byte(v.Address))
		proofs = append(proofs, Proof{
			ValidatorAddr: v.Address,
			Hash:          h,
			Stake:         v.TotalStake,
		})
	}

	sort.Slice(proofs, func(i, j int) bool {
		for k := 0; k < len(proofs[i].Hash) && k < len(proofs[j].Hash); k++ {
			if proofs[i].Hash[k] != proofs[j].Hash[k] {
				return proofs[i].Hash[k] < proofs[j].Hash[k]
			}
		}
		// Tie-break: higher stake wins (lower sort position)
		return proofs[i].Stake > proofs[j].Stake
	})

	if len(proofs) == 0 {
		return "", ""
	}
	proposer = proofs[0].ValidatorAddr
	if len(proofs) > 1 {
		server = proofs[1].ValidatorAddr
	} else {
		// Only one active validator: it serves as both proposer and server
		server = proofs[0].ValidatorAddr
	}
	return proposer, server
}

// EpochSeed derives the epoch seed from the last block hash of the previous epoch.
// In production this comes from the last block header BeginBlock provides.
func EpochSeed(lastBlockHash []byte, epoch int64) []byte {
	epochBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(epochBytes, uint64(epoch))
	return types.Keccak256(lastBlockHash, epochBytes)
}
