package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bigtchain/bigt/chain/modules/checkpoint"
)

// fixedPrivKey returns a deterministic 32-byte private key for tests.
func fixedPrivKey(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	k[0] = seed | 0x01
	return k
}

// -----------------------------------------------------------------------
// ABI encoding tests
// -----------------------------------------------------------------------

func TestEncodeSubmitCheckpoint_Length(t *testing.T) {
	var bh, vsh [32]byte
	bh[0] = 0xde
	vsh[0] = 0xad

	sig := make([]byte, 65)
	signer := checkpoint.AddressFromPrivKey(fixedPrivKey(0x11))

	data, err := encodeSubmitCheckpoint(1, bh, vsh, [][20]byte{signer}, [][]byte{sig})
	if err != nil {
		t.Fatalf("encodeSubmitCheckpoint: %v", err)
	}

	// 4 (selector) + 5*32 (head) + 32+32 (signers:len+1addr) + 32+32+(32+96) (sigs:len+1off+1elem) = ...
	// selector: 4
	// head: 5*32 = 160
	// signers tail: 32 (len) + 1*32 (addr) = 64
	// sigs tail: 32 (len) + 1*32 (sub-offset) + 32 (sig-len) + 96 (sig-padded) = 192
	// total: 4 + 160 + 64 + 192 = 420
	want := 4 + 160 + 64 + 192
	if len(data) != want {
		t.Fatalf("expected %d bytes, got %d", want, len(data))
	}
}

func TestEncodeSubmitCheckpoint_Selector(t *testing.T) {
	var bh, vsh [32]byte
	signer := checkpoint.AddressFromPrivKey(fixedPrivKey(0x22))
	sig := make([]byte, 65)

	data, err := encodeSubmitCheckpoint(1, bh, vsh, [][20]byte{signer}, [][]byte{sig})
	if err != nil {
		t.Fatalf("encodeSubmitCheckpoint: %v", err)
	}

	gotSel := hex.EncodeToString(data[:4])
	// selector for submitCheckpoint(uint64,bytes32,bytes32,address[],bytes[])
	// computed: keccak256("submitCheckpoint(uint64,bytes32,bytes32,address[],bytes[])")[:4]
	// logged for reference:
	t.Logf("submitCheckpoint selector: %s", gotSel)
	if gotSel == "00000000" {
		t.Fatal("selector must not be all zeros")
	}
	// Must be consistent across calls.
	data2, _ := encodeSubmitCheckpoint(1, bh, vsh, [][20]byte{signer}, [][]byte{sig})
	if hex.EncodeToString(data2[:4]) != gotSel {
		t.Fatal("selector must be deterministic")
	}
}

func TestEncodeSubmitCheckpoint_SignerMismatch(t *testing.T) {
	var bh, vsh [32]byte
	signer := checkpoint.AddressFromPrivKey(fixedPrivKey(0x33))
	_, err := encodeSubmitCheckpoint(1, bh, vsh,
		[][20]byte{signer},        // 1 signer
		[][]byte{{}, {0x01}},      // 2 sigs
	)
	if err == nil {
		t.Fatal("expected error for signer/sig count mismatch")
	}
}

func TestEncodeSubmitCheckpoint_SignersSorted(t *testing.T) {
	pk1 := fixedPrivKey(0x10)
	pk2 := fixedPrivKey(0x20)
	a1 := checkpoint.AddressFromPrivKey(pk1)
	a2 := checkpoint.AddressFromPrivKey(pk2)
	sig := make([]byte, 65)
	var bh, vsh [32]byte

	// Pass in reverse order — encoder must sort.
	data, err := encodeSubmitCheckpoint(1, bh, vsh,
		[][20]byte{a2, a1},
		[][]byte{sig, sig},
	)
	if err != nil {
		t.Fatalf("encodeSubmitCheckpoint: %v", err)
	}
	// First address in tail (offset = 4 + 5*32 + 32 = 196).
	firstAddrSlot := data[4+5*32+32 : 4+5*32+32+32]
	// If sorted, the smaller address is first (right-aligned in 32-byte word).
	var smallerAddr [20]byte
	if bytes.Compare(a1[:], a2[:]) < 0 {
		smallerAddr = a1
	} else {
		smallerAddr = a2
	}
	if !strings.EqualFold(hex.EncodeToString(firstAddrSlot[12:]), hex.EncodeToString(smallerAddr[:])) {
		t.Fatalf("signers must be sorted: first addr slot = %x, want %x",
			firstAddrSlot[12:], smallerAddr[:])
	}
}

// -----------------------------------------------------------------------
// parseBlock tests
// -----------------------------------------------------------------------

func TestParseBlock_ValidJSON(t *testing.T) {
	raw := `{
		"result": {
			"block": {
				"header": {
					"height": "14401",
					"last_block_id": {"hash": "deadbeef1234"}
				}
			}
		}
	}`
	height, hash, err := parseBlock([]byte(raw))
	if err != nil {
		t.Fatalf("parseBlock: %v", err)
	}
	if height != 14401 {
		t.Fatalf("expected height 14401, got %d", height)
	}
	if hash != "deadbeef1234" {
		t.Fatalf("expected hash deadbeef1234, got %s", hash)
	}
}

func TestParseBlock_BadJSON(t *testing.T) {
	_, _, err := parseBlock([]byte("notjson{{{"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

// -----------------------------------------------------------------------
// ParseHexKey tests
// -----------------------------------------------------------------------

func TestParseHexKey_Valid(t *testing.T) {
	pk := fixedPrivKey(0x01)
	hex := fmt.Sprintf("%x", pk)
	got, err := ParseHexKey(hex)
	if err != nil {
		t.Fatalf("ParseHexKey: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(got))
	}
}

func TestParseHexKey_TooShort(t *testing.T) {
	_, err := ParseHexKey("deadbeef")
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestParseHexKey_NotHex(t *testing.T) {
	_, err := ParseHexKey(strings.Repeat("GG", 32))
	if err == nil {
		t.Fatal("expected error for non-hex input")
	}
}

// -----------------------------------------------------------------------
// Mock L1Submitter
// -----------------------------------------------------------------------

type mockSubmitter struct {
	calls  atomic.Int32
	lastEp uint64
	err    error
}

func (m *mockSubmitter) Submit(_ context.Context, cp *checkpoint.Checkpoint, _ [][20]byte, _ [][]byte) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.calls.Add(1)
	m.lastEp = cp.Epoch
	return "0xdeadbeef", nil
}

// -----------------------------------------------------------------------
// Relayer.poll tests (with mock CometBFT server)
// -----------------------------------------------------------------------

func makeCMTServer(height uint64, blockHash string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"result": map[string]any{
				"block": map[string]any{
					"header": map[string]any{
						"height": fmt.Sprintf("%d", height),
						"last_block_id": map[string]string{
							"hash": blockHash,
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestRelayer_Poll_SubmitsAtEpochBoundary(t *testing.T) {
	// EpochSlots = 100; height = 101 → epoch 1 boundary.
	blockHash := strings.Repeat("ab", 32) // 64 hex chars = 32 bytes
	srv := makeCMTServer(101, blockHash)
	defer srv.Close()

	sub := &mockSubmitter{}
	r := New(Config{
		CMTRPCURL:    srv.URL,
		EpochSlots:   100,
		PrivKey:      fixedPrivKey(0x01),
		PollInterval: time.Hour, // won't fire; we call poll directly
	}, sub)

	if err := r.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if sub.calls.Load() != 1 {
		t.Fatalf("expected 1 submission, got %d", sub.calls.Load())
	}
	if sub.lastEp != 1 {
		t.Fatalf("expected epoch 1, got %d", sub.lastEp)
	}
}

func TestRelayer_Poll_NoSubmitBeforeFirstEpoch(t *testing.T) {
	// height = 50, epochSlots = 100 → epoch 0 → skip.
	srv := makeCMTServer(50, strings.Repeat("00", 32))
	defer srv.Close()

	sub := &mockSubmitter{}
	r := New(Config{
		CMTRPCURL:  srv.URL,
		EpochSlots: 100,
		PrivKey:    fixedPrivKey(0x01),
	}, sub)

	if err := r.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if sub.calls.Load() != 0 {
		t.Fatalf("expected 0 submissions before epoch 1, got %d", sub.calls.Load())
	}
}

func TestRelayer_Poll_NoDoubleSubmit(t *testing.T) {
	blockHash := strings.Repeat("cc", 32)
	srv := makeCMTServer(101, blockHash)
	defer srv.Close()

	sub := &mockSubmitter{}
	r := New(Config{
		CMTRPCURL:  srv.URL,
		EpochSlots: 100,
		PrivKey:    fixedPrivKey(0x01),
	}, sub)

	_ = r.poll(context.Background())
	_ = r.poll(context.Background()) // second call — same epoch, no new submission
	if sub.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 submission, got %d", sub.calls.Load())
	}
}

func TestRelayer_Poll_SubmitterError_Propagates(t *testing.T) {
	srv := makeCMTServer(101, strings.Repeat("dd", 32))
	defer srv.Close()

	sub := &mockSubmitter{err: fmt.Errorf("l1 down")}
	r := New(Config{
		CMTRPCURL:  srv.URL,
		EpochSlots: 100,
		PrivKey:    fixedPrivKey(0x01),
	}, sub)

	err := r.poll(context.Background())
	if err == nil {
		t.Fatal("expected error when submitter fails")
	}
}

func TestRelayer_Poll_BadCMTResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	sub := &mockSubmitter{}
	r := New(Config{
		CMTRPCURL:  srv.URL,
		EpochSlots: 100,
		PrivKey:    fixedPrivKey(0x01),
	}, sub)

	err := r.poll(context.Background())
	if err == nil {
		t.Fatal("expected error for bad CMT response")
	}
}

// -----------------------------------------------------------------------
// HTTPSubmitter.Submit integration (against a mock L1 JSON-RPC server)
// -----------------------------------------------------------------------

func TestHTTPSubmitter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xaabb1234",
		})
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "0x1234567890123456789012345678901234567890")
	cp := checkpoint.New(1, [32]byte{0x01}, [32]byte{0x02})
	pk := fixedPrivKey(0x01)
	sig, _ := cp.Sign(pk)
	signer := checkpoint.AddressFromPrivKey(pk)

	txHash, err := s.Submit(context.Background(), cp, [][20]byte{signer}, [][]byte{sig})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	if txHash != "0xaabb1234" {
		t.Fatalf("expected tx hash 0xaabb1234, got %s", txHash)
	}
}

func TestHTTPSubmitter_RPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]any{
				"code":    -32000,
				"message": "insufficient funds",
			},
		})
	}))
	defer srv.Close()

	s := NewHTTPSubmitter(srv.URL, "0x0000000000000000000000000000000000000001")
	cp := checkpoint.New(1, [32]byte{}, [32]byte{})
	pk := fixedPrivKey(0x01)
	sig, _ := cp.Sign(pk)
	signer := checkpoint.AddressFromPrivKey(pk)

	_, err := s.Submit(context.Background(), cp, [][20]byte{signer}, [][]byte{sig})
	if err == nil {
		t.Fatal("expected error for RPC error response")
	}
}
