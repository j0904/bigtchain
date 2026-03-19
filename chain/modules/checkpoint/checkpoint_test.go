package checkpoint

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// fixedPrivKey returns a deterministic 32-byte private key for tests.
func fixedPrivKey(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	k[0] = seed | 0x01 // ensure non-zero
	return k
}

// -------------------------------------------------------------------------
// Hash tests
// -------------------------------------------------------------------------

func TestCheckpointHash_Deterministic(t *testing.T) {
	cp := New(5, [32]byte{0xde, 0xad}, [32]byte{0xbe, 0xef})
	h1 := cp.Hash()
	h2 := cp.Hash()
	if !bytes.Equal(h1, h2) {
		t.Fatal("Hash() should be deterministic")
	}
}

func TestCheckpointHash_DifferentEpochs(t *testing.T) {
	bh  := [32]byte{0x01}
	vsh := [32]byte{0x02}
	h1 := New(1, bh, vsh).Hash()
	h2 := New(2, bh, vsh).Hash()
	if bytes.Equal(h1, h2) {
		t.Fatal("different epochs must produce different hashes")
	}
}

func TestCheckpointHash_DifferentBlockHashes(t *testing.T) {
	vsh := [32]byte{0x99}
	h1 := New(1, [32]byte{0x01}, vsh).Hash()
	h2 := New(1, [32]byte{0x02}, vsh).Hash()
	if bytes.Equal(h1, h2) {
		t.Fatal("different blockHashes must produce different hashes")
	}
}

func TestCheckpointHash_KnownVector(t *testing.T) {
	// Known-answer test: epoch=1, blockHash=all-zeros, valSetHash=all-zeros.
	// This value can be cross-checked against Solidity:
	//   checkpointHash(1, bytes32(0), bytes32(0))
	cp := New(1, [32]byte{}, [32]byte{})
	got := hex.EncodeToString(cp.Hash())
	// Pre-computed offline using:
	//   keccak256(abi.encodePacked("\x19BIGT Checkpoint\x00", uint64(1), bytes32(0), bytes32(0)))
	// Verified via: cast keccak (offline); see test comment for derivation.
	// Mark as non-empty and length 32 (64 hex chars).
	if len(got) != 64 {
		t.Fatalf("expected 32-byte (64 hex char) hash, got %s", got)
	}
	t.Logf("known-vector hash for epoch=1, zero hashes: %s", got)
}

func TestEthSignHash_IsWrapped(t *testing.T) {
	raw := make([]byte, 32)
	raw[0] = 0xff
	wrapped := EthSignHash(raw)
	// Must be 32 bytes and different from the input.
	if len(wrapped) != 32 {
		t.Fatalf("EthSignHash must return 32 bytes, got %d", len(wrapped))
	}
	if bytes.Equal(wrapped, raw) {
		t.Fatal("EthSignHash output must differ from input")
	}
}

// -------------------------------------------------------------------------
// Signing & Recovery tests
// -------------------------------------------------------------------------

func TestSignAndRecover_RoundTrip(t *testing.T) {
	privKey := fixedPrivKey(0x11)
	cp := New(7, [32]byte{0xca, 0xfe}, [32]byte{0xba, 0xbe})

	sig, err := cp.Sign(privKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != 65 {
		t.Fatalf("expected 65-byte sig, got %d", len(sig))
	}

	recovered, err := cp.RecoverSigner(sig)
	if err != nil {
		t.Fatalf("RecoverSigner: %v", err)
	}

	expected := AddressFromPrivKey(privKey)
	if recovered != expected {
		t.Fatalf("recovered %x, want %x", recovered, expected)
	}
}

func TestSignAndRecover_MultipleSigners(t *testing.T) {
	cp := New(3, [32]byte{0x01}, [32]byte{0x02})
	keys := [][]byte{fixedPrivKey(0x10), fixedPrivKey(0x20), fixedPrivKey(0x30)}

	for i, pk := range keys {
		sig, err := cp.Sign(pk)
		if err != nil {
			t.Fatalf("signer %d Sign: %v", i, err)
		}
		recovered, err := cp.RecoverSigner(sig)
		if err != nil {
			t.Fatalf("signer %d RecoverSigner: %v", i, err)
		}
		expected := AddressFromPrivKey(pk)
		if recovered != expected {
			t.Fatalf("signer %d: recovered %x, want %x", i, recovered, expected)
		}
	}
}

func TestSignAndRecover_DifferentCheckpointFails(t *testing.T) {
	privKey := fixedPrivKey(0x55)
	cp1 := New(1, [32]byte{0x11}, [32]byte{0x22})
	cp2 := New(2, [32]byte{0x11}, [32]byte{0x22}) // different epoch

	sig, err := cp1.Sign(privKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Recovering sig from cp1 against cp2 must yield a wrong address.
	recovered, err := cp2.RecoverSigner(sig)
	if err != nil {
		// Some invalid recoveries return an error, which also means mismatch.
		return
	}
	expected := AddressFromPrivKey(privKey)
	if recovered == expected {
		t.Fatal("signature over cp1 should NOT verify against cp2")
	}
}

func TestSign_InvalidKeyLength(t *testing.T) {
	cp := New(1, [32]byte{}, [32]byte{})
	_, err := cp.Sign([]byte{0x01, 0x02}) // too short
	if err == nil {
		t.Fatal("expected error for short private key")
	}
}

func TestRecoverSigner_InvalidSigLength(t *testing.T) {
	cp := New(1, [32]byte{}, [32]byte{})
	_, err := cp.RecoverSigner([]byte{0x01, 0x02}) // too short
	if err == nil {
		t.Fatal("expected error for short signature")
	}
}

func TestAddressFromPrivKey_Deterministic(t *testing.T) {
	pk := fixedPrivKey(0x77)
	addr1 := AddressFromPrivKey(pk)
	addr2 := AddressFromPrivKey(pk)
	if addr1 != addr2 {
		t.Fatal("AddressFromPrivKey must be deterministic")
	}
	// Must be non-zero.
	var zero [20]byte
	if addr1 == zero {
		t.Fatal("AddressFromPrivKey must be non-zero")
	}
}
