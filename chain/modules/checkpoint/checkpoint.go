// Package checkpoint implements epoch checkpoint hashing and signing for the
// BIGT chain L1 anchor.  The hash function mirrors the Solidity
// _checkpointHash in contracts/src/CheckpointAnchor.sol exactly so that
// signatures produced here are verifiable on-chain.
package checkpoint

import (
	"encoding/binary"
	"fmt"

	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	secp_ecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/bigtchain/bigt/chain/types"
)

// domainPrefix is the EIP-191-style domain prefix for checkpoint messages.
// It must match the literal in CheckpointAnchor.sol _checkpointHash.
const domainPrefix = "\x19BIGT Checkpoint\x00"

// Checkpoint holds the fields anchored at each epoch boundary.
type Checkpoint struct {
	Epoch            uint64
	BlockHash        [32]byte
	ValidatorSetHash [32]byte
}

// New creates a Checkpoint from the given epoch and hashes.
func New(epoch uint64, blockHash, validatorSetHash [32]byte) *Checkpoint {
	return &Checkpoint{
		Epoch:            epoch,
		BlockHash:        blockHash,
		ValidatorSetHash: validatorSetHash,
	}
}

// Hash returns the raw checkpoint hash:
//
//	keccak256("\x19BIGT Checkpoint\x00" || epoch_be8 || blockHash || validatorSetHash)
//
// This mirrors the Solidity _checkpointHash function exactly.  The epoch is
// encoded as a big-endian uint64 (8 bytes), identical to abi.encodePacked.
func (cp *Checkpoint) Hash() []byte {
	epochBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(epochBuf, cp.Epoch)
	return types.Keccak256(
		[]byte(domainPrefix),
		epochBuf,
		cp.BlockHash[:],
		cp.ValidatorSetHash[:],
	)
}

// EthSignHash wraps a raw 32-byte hash with the EIP-191 personal_sign prefix:
//
//	keccak256("\x19Ethereum Signed Message:\n32" || hash)
//
// This matches the Solidity _verifyRelayerSignatures eth hash computation.
func EthSignHash(cpHash []byte) []byte {
	return types.Keccak256(
		[]byte("\x19Ethereum Signed Message:\n32"),
		cpHash,
	)
}

// Sign produces a 65-byte secp256k1 ECDSA signature over this checkpoint using
// EIP-191 personal_sign encoding, matching the Solidity ecrecover call.
//
// Format: r (32 bytes) || s (32 bytes) || v (1 byte, 27 or 28).
//
// privKeyBytes must be a 32-byte secp256k1 scalar.
func (cp *Checkpoint) Sign(privKeyBytes []byte) ([]byte, error) {
	if len(privKeyBytes) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes, got %d", len(privKeyBytes))
	}
	ethHash := EthSignHash(cp.Hash())

	privKey := secp.PrivKeyFromBytes(privKeyBytes)
	// SignCompact returns [v || r || s] with v = 27 or 28.
	compact := secp_ecdsa.SignCompact(privKey, ethHash, false)
	if len(compact) != 65 {
		return nil, fmt.Errorf("unexpected compact signature length %d", len(compact))
	}
	// Reorder to Ethereum format [r || s || v].
	sig := make([]byte, 65)
	copy(sig[0:32], compact[1:33])  // r
	copy(sig[32:64], compact[33:65]) // s
	sig[64] = compact[0]             // v
	return sig, nil
}

// RecoverSigner recovers the 20-byte Ethereum address that produced sig over
// this checkpoint.  sig must be 65 bytes in [r || s || v] format.
func (cp *Checkpoint) RecoverSigner(sig []byte) ([20]byte, error) {
	if len(sig) != 65 {
		return [20]byte{}, fmt.Errorf("signature must be 65 bytes, got %d", len(sig))
	}
	ethHash := EthSignHash(cp.Hash())

	// Reorder from [r || s || v] to [v || r || s] for RecoverCompact.
	compact := make([]byte, 65)
	compact[0] = sig[64]            // v
	copy(compact[1:33], sig[0:32])  // r
	copy(compact[33:65], sig[32:64]) // s

	pubKey, _, err := secp_ecdsa.RecoverCompact(compact, ethHash)
	if err != nil {
		return [20]byte{}, fmt.Errorf("recover failed: %w", err)
	}
	return pubKeyToAddress(pubKey), nil
}

// AddressFromPrivKey returns the Ethereum address corresponding to a 32-byte
// secp256k1 private key scalar.
func AddressFromPrivKey(privKeyBytes []byte) [20]byte {
	privKey := secp.PrivKeyFromBytes(privKeyBytes)
	return pubKeyToAddress(privKey.PubKey())
}

// pubKeyToAddress converts a secp256k1 public key to an Ethereum address.
// Ethereum address = keccak256(uncompressed_pubkey[1:])[12:].
func pubKeyToAddress(pubKey *secp.PublicKey) [20]byte {
	uncompressed := pubKey.SerializeUncompressed() // 65 bytes: 0x04 || x || y
	addrHash := types.Keccak256(uncompressed[1:])  // keccak256(x || y), 32 bytes
	var addr [20]byte
	copy(addr[:], addrHash[12:]) // take last 20 bytes
	return addr
}
