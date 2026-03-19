# BIGT — Proof of Useful Stake

A CometBFT-based blockchain for decentralised AI inference. Validators earn rewards by correctly routing AI job requests to inference backends and committing to outputs on-chain. No GPUs required at the protocol level — validators are economically accountable service providers, not compute providers.

---

## How it works

Each slot (6 seconds) a VRF-elected proposer and three worker validators are selected. A user submits an AI job (prompt + model ID). The three workers:

1. Route the job to their inference backend (any OpenAI-compatible endpoint)
2. Hash the output: `keccak256(output_tokens || job_id || validator_pubkey)`
3. Submit the commitment on-chain (commit phase)
4. Reveal the plaintext output (reveal phase)

If 2-of-3 commitments match, the job is accepted and all three are rewarded. Mismatches trigger a dispute window. Users who receive an incorrect output can dispute on-chain; the hash is recomputed and the lying validator is slashed 100%.

Every epoch (~24 hours, 14 400 slots) a checkpoint is anchored to an EVM L1 contract for weak subjectivity.

---

## Protocol constants

| Parameter | Value |
|---|---|
| Slot duration | 6 s |
| Epoch length | 14 400 slots (~24 h) |
| Workers per job | 3 |
| Min validator stake | 5 000 BIGT |
| Max validator stake | 5% of total supply |
| Unbonding period | 21 days |
| Mismatch slash | 10% of bond |
| Malicious slash | 100% of bond |
| Inactivity threshold | 4 consecutive missed epochs |
| Dispute window | 7 slots |

---

## Repository layout

```
chain/
  types/           Protocol types, constants, Keccak256 helper
  genesis/         Genesis document (load/save JSON)
  store/           LevelDB-backed KV store with prefix scanning
  app/             CometBFT ABCI application (FinalizeBlock)
  registry/        On-chain model registry with governance votes
  modules/
    staking/       Validator registration, bonding, slashing, unbonding
    vrf/           Deterministic slot & worker election (keccak sort)
    jobs/          3-phase commit/reveal/commitment job pipeline
    slashing/      Dispute handler (user dispute, mismatch, equivocation)
    rewards/       Epoch reward distribution (inflation + job fees)
    checkpoint/    Epoch checkpoint hashing and ECDSA signing

cmd/
  bigt/            Node binary  (bigt --init / bigt)
  validator/       Validator router client
  relayer/         L1 checkpoint relayer

validator/
  listener/        CometBFT mempool watcher
  router/          OpenAI-compatible HTTP inference router
  broadcaster/     On-chain commitment broadcaster

contracts/
  src/
    CheckpointAnchor.sol   L1 checkpoint anchor (ECDSA multisig)
  test/
    CheckpointAnchor.t.sol Foundry tests

tests/
  integration/     Full slot lifecycle, dispute, and rewards integration tests
```

---

## Getting started

### Prerequisites

| Tool | Version |
|---|---|
| Go | 1.23.6 |
| Foundry | 1.5+ (`foundryup`) |

### Build

```bash
# Node
go build ./cmd/bigt

# Validator client
go build ./cmd/validator

# L1 relayer
go build ./cmd/relayer
```

### Run tests

```bash
# Go — all modules + integration
go test ./chain/... ./cmd/relayer/... ./tests/...

# Solidity
cd contracts && forge test -v
```

### Start a local devnet

```bash
# Initialise genesis and config
./bigt --init --home ~/.bigt

# Run the node
./bigt --home ~/.bigt

# Run a validator client (in a separate terminal)
./validator \
  --key <32-byte-hex-privkey> \
  --backend http://localhost:11434/v1  # any OpenAI-compatible endpoint
```

---

## Validator setup

A validator needs three things:

**1. A staked account** — register with at least 5 000 BIGT bonded.

**2. An inference backend** — any server that speaks the OpenAI chat completions API:
- [Ollama](https://ollama.com) (local)
- [llama.cpp server](https://github.com/ggerganov/llama.cpp) (local)
- [Together.ai](https://together.ai), [Fireworks.ai](https://fireworks.ai), [Groq](https://groq.com) (cloud APIs)
- [RunPod](https://runpod.io), [Vast.ai](https://vast.ai) (rented GPUs)

**3. A relayer key** (optional) — to participate in anchoring epoch checkpoints to L1.

The validator client (`cmd/validator`) handles chain listening, job routing, and commitment broadcasting. It targets a 3-second deadline per job with one automatic retry.

---

## L1 checkpoint anchor

`contracts/src/CheckpointAnchor.sol` accepts epoch checkpoints from a permissioned relayer set using ECDSA multisig (quorum-of-N). Each checkpoint commits:

```
keccak256("\x19BIGT Checkpoint\x00" || epoch || blockHash || validatorSetHash)
```

The Go relayer (`cmd/relayer`) polls CometBFT for epoch boundaries, signs checkpoints with the configured key, and submits them via `eth_sendTransaction`.

```bash
./relayer \
  --cmt  http://localhost:26657 \
  --l1   http://localhost:8545 \
  --contract 0xYourCheckpointAnchorAddress \
  --key  <32-byte-hex-privkey>
```

---

## Security model

- **Economic accountability**: validators post a bond slashed for incorrect outputs, not for wrong computation strategy.
- **Redundant execution**: 3 independent validators answer every job; 2-of-3 agreement is required.
- **Determinism**: registered models use fixed weights hash + temperature=0; divergence is evidence of cheating.
- **User disputes**: users hold the plaintext output and can prove on-chain if a commitment was false.
- **Weak subjectivity**: epoch checkpoints on L1 prevent long-range validator-set rewriting attacks.

---

## Test coverage

| Package | Tests |
|---|---|
| `chain/modules/staking` | 8 |
| `chain/modules/vrf` | 7 |
| `chain/modules/jobs` | 7 |
| `chain/modules/rewards` | 3 |
| `chain/modules/checkpoint` | 11 |
| `chain/registry` | 4 |
| `cmd/relayer` | 16 |
| `tests/integration` | 4 |
| `contracts/` (Solidity) | 14 |
| **Total** | **74** |

---

## License

MIT
