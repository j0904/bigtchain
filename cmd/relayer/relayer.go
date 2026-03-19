// relayer.go contains the core checkpoint-relaying logic: watching CometBFT for
// epoch boundaries and submitting proofs to the L1 CheckpointAnchor contract.
//
// Architecture
//
//	CometBFT RPC ──► Relayer.Run() ──► sign checkpoint ──► L1Submitter.Submit()
//
// The L1Submitter interface allows easy swapping of the real Ethereum JSON-RPC
// backend with a mock in tests.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/bigtchain/bigt/chain/modules/checkpoint"
	"github.com/bigtchain/bigt/chain/types"
)

// ParseHexKey decodes a 64-character hex string into a 32-byte private key.
func ParseHexKey(hexStr string) ([]byte, error) {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(b))
	}
	return b, nil
}

// -----------------------------------------------------------------------
// L1Submitter interface
// -----------------------------------------------------------------------

// L1Submitter abstracts L1 checkpoint submission.  The real implementation
// encodes the calldata for CheckpointAnchor.submitCheckpoint and sends it
// via eth_sendTransaction; tests inject a mock.
type L1Submitter interface {
	// Submit sends the checkpoint to the L1 contract.
	// Returns the L1 tx hash (hex) or an error.
	Submit(ctx context.Context, cp *checkpoint.Checkpoint, signers [][20]byte, sigs [][]byte) (string, error)
}

// -----------------------------------------------------------------------
// CometBFT client (minimal)
// -----------------------------------------------------------------------

// cmtBlock is a minimal deserialisation of a CometBFT /block JSON response.
type cmtBlock struct {
	Result struct {
		Block struct {
			Header struct {
				Height          string `json:"height"`
				LastBlockID struct {
					Hash string `json:"hash"`
				} `json:"last_block_id"`
			} `json:"header"`
		} `json:"block"`
	} `json:"result"`
}

// -----------------------------------------------------------------------
// Relayer
// -----------------------------------------------------------------------

// Config holds relayer configuration.
type Config struct {
	// CometBFT JSON-RPC endpoint.
	CMTRPCURL string

	// Epoch size in slots / blocks.
	EpochSlots uint64

	// 32-byte secp256k1 private key for signing checkpoints.
	PrivKey []byte

	// How often to poll CometBFT for new blocks.
	PollInterval time.Duration
}

// Relayer watches the chain and submits epoch checkpoints to L1.
type Relayer struct {
	cfg         Config
	submitter   L1Submitter
	httpClient  *http.Client
	lastEpoch   uint64
}

// New creates a Relayer.
func New(cfg Config, submitter L1Submitter) *Relayer {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &Relayer{
		cfg:       cfg,
		submitter: submitter,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Run blocks, polling CometBFT and submitting checkpoints.  Cancel ctx to stop.
func (r *Relayer) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.poll(ctx); err != nil {
				// Log and continue; transient errors are expected.
				fmt.Printf("relayer poll error: %v\n", err)
			}
		}
	}
}

// poll fetches the latest block header and, if an epoch boundary has been
// crossed since our last submission, builds and submits a checkpoint.
func (r *Relayer) poll(ctx context.Context) error {
	blk, err := r.latestBlock(ctx)
	if err != nil {
		return fmt.Errorf("latestBlock: %w", err)
	}
	height, blockHashHex, err := parseBlock(blk)
	if err != nil {
		return fmt.Errorf("parseBlock: %w", err)
	}

	epoch := height / r.cfg.EpochSlots
	if epoch == 0 || epoch <= r.lastEpoch {
		return nil // not yet at first epoch boundary or already submitted
	}

	blockHashBytes, err := hex.DecodeString(blockHashHex)
	if err != nil {
		return fmt.Errorf("decode blockHash: %w", err)
	}
	var blockHash [32]byte
	copy(blockHash[:], blockHashBytes)

	// Validator-set hash: in production derive from on-chain staking state.
	// For this relayer we use keccak256(epoch) as a placeholder until
	// the chain exposes a stable validator-set commitment per epoch.
	valSetHash := computeValSetHash(epoch)

	cp := checkpoint.New(epoch, blockHash, valSetHash)
	sig, err := cp.Sign(r.cfg.PrivKey)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}

	signer := checkpoint.AddressFromPrivKey(r.cfg.PrivKey)
	txHash, err := r.submitter.Submit(ctx, cp, [][20]byte{signer}, [][]byte{sig})
	if err != nil {
		return fmt.Errorf("submit epoch %d: %w", epoch, err)
	}

	fmt.Printf("anchored epoch %d → L1 tx %s\n", epoch, txHash)
	r.lastEpoch = epoch
	return nil
}

// computeValSetHash derives a placeholder validator-set hash from the epoch.
// Production code would derive this from the ABCI state (ValidatorUpdates).
func computeValSetHash(epoch uint64) [32]byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, epoch)
	h := types.Keccak256([]byte("valset"), buf)
	var out [32]byte
	copy(out[:], h)
	return out
}

func (r *Relayer) latestBlock(ctx context.Context) ([]byte, error) {
	url := r.cfg.CMTRPCURL + "/block"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// parseBlock extracts (height, lastBlockHash) from a CometBFT /block response.
func parseBlock(data []byte) (height uint64, blockHashHex string, err error) {
	var blk cmtBlock
	if err = json.Unmarshal(data, &blk); err != nil {
		return
	}
	fmt.Sscanf(blk.Result.Block.Header.Height, "%d", &height)
	blockHashHex = blk.Result.Block.Header.LastBlockID.Hash
	return
}

// -----------------------------------------------------------------------
// HTTP L1 Submitter (raw JSON-RPC)
// -----------------------------------------------------------------------

// HTTPSubmitter sends the checkpoint to L1 using raw eth_sendTransaction
// JSON-RPC (assumes an unlocked relayer account on the node, or use a
// pre-funded personal_sendTransaction with explicit key management via
// a production signer service).
type HTTPSubmitter struct {
	l1RPCURL     string
	contractAddr string // hex with 0x prefix
	httpClient   *http.Client
}

// NewHTTPSubmitter creates an HTTPSubmitter.
func NewHTTPSubmitter(l1RPCURL, contractAddr string) *HTTPSubmitter {
	return &HTTPSubmitter{
		l1RPCURL:     l1RPCURL,
		contractAddr: contractAddr,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

// Submit encodes the submitCheckpoint calldata and sends it to L1.
func (s *HTTPSubmitter) Submit(ctx context.Context, cp *checkpoint.Checkpoint, signers [][20]byte, sigs [][]byte) (string, error) {
	calldata, err := encodeSubmitCheckpoint(cp.Epoch, cp.BlockHash, cp.ValidatorSetHash, signers, sigs)
	if err != nil {
		return "", fmt.Errorf("encode calldata: %w", err)
	}

	params := map[string]string{
		"to":   s.contractAddr,
		"data": "0x" + hex.EncodeToString(calldata),
		"gas":  "0x100000",
	}
	return s.ethSendTransaction(ctx, params)
}

// ethSendTransaction sends a JSON-RPC eth_sendTransaction call and returns the
// resulting tx hash.
func (s *HTTPSubmitter) ethSendTransaction(ctx context.Context, params map[string]string) (string, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_sendTransaction",
		"params":  []any{params},
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.l1RPCURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Error != nil {
		return "", fmt.Errorf("rpc error: %s", result.Error.Message)
	}
	return result.Result, nil
}

// -----------------------------------------------------------------------
// Minimal ABI encoder for submitCheckpoint
// -----------------------------------------------------------------------
//
// ABI encoding of:
//   submitCheckpoint(uint64 epoch, bytes32 blockHash, bytes32 valSetHash,
//                    address[] signers, bytes[] sigs)
//
// Function selector: keccak256("submitCheckpoint(uint64,bytes32,bytes32,address[],bytes[])")[:4]
//
// Head layout (5 × 32 bytes):
//   slot 0: epoch          (uint64, right-aligned)
//   slot 1: blockHash      (bytes32)
//   slot 2: valSetHash     (bytes32)
//   slot 3: offset to signers[] data = 5*32 = 160
//   slot 4: offset to sigs[]    data = 160 + 32 + len(signers)*32
//
// Tail:
//   signers[]: length word + padded addresses
//   sigs[]:    length word + per-element offsets + per-element (length word + padded data)

// selector for submitCheckpoint(uint64,bytes32,bytes32,address[],bytes[])
var submitCheckpointSelector = func() [4]byte {
	b := types.Keccak256([]byte("submitCheckpoint(uint64,bytes32,bytes32,address[],bytes[])"))
	var sel [4]byte
	copy(sel[:], b[:4])
	return sel
}()

func encodeSubmitCheckpoint(
	epoch uint64,
	blockHash, valSetHash [32]byte,
	signers [][20]byte,
	sigs [][]byte,
) ([]byte, error) {
	if len(signers) != len(sigs) {
		return nil, fmt.Errorf("signers/sigs length mismatch")
	}

	// Ensure signers are sorted ascending (contract requirement).
	paired := make([]struct {
		addr [20]byte
		sig  []byte
	}, len(signers))
	for i := range signers {
		paired[i] = struct {
			addr [20]byte
			sig  []byte
		}{signers[i], sigs[i]}
	}
	sort.Slice(paired, func(i, j int) bool {
		return bytes.Compare(paired[i].addr[:], paired[j].addr[:]) < 0
	})

	n := len(signers)

	// Compute offsets.
	// Head is 5 × 32 bytes.
	headSize := 5 * 32
	// signers[] tail: 32 (length) + n*32 (elements).
	signersDataSize := 32 + n*32
	// sigs[] tail: 32 (length) + n*32 (sub-offsets) + n*(32+64) each (32 len + 64 data, padded to 32*ceil(64/32)=64).
	// Signature is 65 bytes → padded to ceil(65/32)*32 = 96 bytes.
	sigDataPaddedSize := ((65 + 31) / 32) * 32 // 96

	signersOffset := headSize                                          // 160
	sigsOffset := headSize + signersDataSize                           // 160 + 32+n*32

	// Build calldata.
	var buf bytes.Buffer
	buf.Write(submitCheckpointSelector[:])

	write32 := func(val []byte) {
		word := make([]byte, 32)
		copy(word[32-len(val):], val)
		buf.Write(word)
	}
	write32Bytes32 := func(b [32]byte) { buf.Write(b[:]) }

	// slot 0: epoch
	epochBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(epochBuf, epoch)
	write32(epochBuf)

	// slot 1: blockHash
	write32Bytes32(blockHash)

	// slot 2: valSetHash
	write32Bytes32(valSetHash)

	// slot 3: offset to signers[]
	off3 := make([]byte, 8)
	binary.BigEndian.PutUint64(off3, uint64(signersOffset))
	write32(off3)

	// slot 4: offset to sigs[]
	off4 := make([]byte, 8)
	binary.BigEndian.PutUint64(off4, uint64(sigsOffset))
	write32(off4)

	// signers[] tail: length + elements.
	lenBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(lenBuf, uint64(n))
	write32(lenBuf)
	for _, p := range paired {
		write32(p.addr[:])
	}

	// sigs[] tail: length + sub-offsets + data.
	binary.BigEndian.PutUint64(lenBuf, uint64(n))
	write32(lenBuf)
	// Sub-offsets: each element's data starts at:
	//   after the length word (32) + after all sub-offset words (n*32).
	subOffsetBase := (1+n)*32
	for i := 0; i < n; i++ {
		subOff := make([]byte, 8)
		binary.BigEndian.PutUint64(subOff, uint64(subOffsetBase+i*(32+sigDataPaddedSize)))
		write32(subOff)
	}
	for _, p := range paired {
		// length of this bytes element
		binary.BigEndian.PutUint64(lenBuf, 65)
		write32(lenBuf)
		// data, right-padded to sigDataPaddedSize
		padded := make([]byte, sigDataPaddedSize)
		copy(padded, p.sig)
		buf.Write(padded)
	}

	return buf.Bytes(), nil
}
