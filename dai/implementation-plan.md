# BIGT — Implementation Plan

Source spec: [claude.md](claude.md)

---

## Project structure

```
bigtchain/
├── chain/               # ABCI application (Go) — CometBFT integration
│   ├── app/             # Application state machine
│   ├── abci/            # ABCI handlers (InitChain, BeginBlock, DeliverTx, EndBlock)
│   ├── modules/
│   │   ├── staking/     # Validator registration, bonding, unbonding
│   │   ├── slashing/    # Penalty logic, equivocation
│   │   ├── vrf/         # VRF-based worker election per slot
│   │   ├── commitreveal/# Commit-reveal for MEV protection
│   │   ├── jobs/        # AI job mempool, 3-phase slot pipeline
│   │   ├── dispute/     # 7-slot challenge window, user proofs
│   │   └── rewards/     # Per-epoch BIGT reward distribution
│   ├── registry/        # ModelRegistry (weights hash, governance approval)
│   └── genesis/         # Genesis file, initial validator set, token supply
├── validator/           # Validator router client (~800 lines, Go or Rust)
│   ├── listener/        # Chain listener: mempool subscription via JSON-RPC
│   ├── router/          # Inference router: OpenAI-compatible HTTP client
│   └── broadcaster/     # Commitment broadcaster: BLS sign + submit tx
├── contracts/           # Solidity contracts for L1 checkpoint anchoring
│   └── CheckpointAnchor.sol
├── proto/               # Protobuf definitions (job request, commitment, dispute)
├── tests/
│   ├── unit/
│   └── integration/
└── scripts/             # Genesis generation, testnet bootstrap
```

---

## Phases

### Phase 1 — Core chain scaffold

**Goal:** Runnable CometBFT node with BIGT genesis and basic block production.

Tasks:
- [ ] Initialise Go module (`github.com/bigtchain/bigt`); pin CometBFT v0.38.x
- [ ] Define genesis: total supply, initial validator set, min stake 5,000 BIGT
- [ ] Implement ABCI `Application` skeleton (InitChain, Info, Query, BeginBlock, EndBlock, Commit)
- [ ] Wire slot timing: target 6-second block interval in CometBFT `config.toml`
- [ ] Define core protobuf types:
  - `JobRequest { job_id, model_id, prompt_commitment, user_address, params }`
  - `OutputCommitment { job_id, validator_pubkey, output_hash, slot, signature }`
  - `DisputeTx { job_id, validator_pubkey, plaintext_output }`
- [ ] Single-node devnet boots and produces blocks

Deliverable: `chain/` compiles, single-node devnet produces 6-second blocks.

---

### Phase 2 — Staking module

**Goal:** Validators can register, bond BIGT, and be tracked for the active set.

Tasks:
- [ ] `MsgRegisterValidator`: pubkey (BLS + consensus), bond amount ≥ 5,000 BIGT, router endpoint metadata
- [ ] `MsgDelegate` / `MsgUndelegate`: delegator bonding; 21-day unbonding queue processed in EndBlock
- [ ] Enforce 5% cap: no single validator may hold > 5% of total stake
- [ ] KES (key-evolving signature) key rotation: validators submit new forward-secret keys each epoch; old keys rejected after the epoch boundary
- [ ] `ValidatorSet` state: active / jailed / unbonding status
- [ ] Epoch boundary (every 14,400 slots ≈ 24h): snapshot active set, compute voting power

Deliverable: Validators can join/leave the active set; stake snapshots are correct at epoch boundaries.

---

### Phase 3 — VRF slot election

**Goal:** Each slot deterministically elects 1 proposer + 3 worker validators using VRF.

Tasks:
- [ ] Each validator in the active set computes `VRF_prove(slot_seed, validator_sk)` off-chain and includes the VRF proof in their block proposal
- [ ] `slot_seed = keccak256(epoch_seed || slot_number)` where `epoch_seed` is committed at epoch start
- [ ] Sort validators by `VRF_hash(proof)` mod stake; lowest hash wins proposer; next-three win worker slots
- [ ] On-chain `VRF_verify` in BeginBlock: reject block if proposer proof invalid
- [ ] Worker slot assignment published in BeginBlock so clients know which validators will serve jobs in this slot
- [ ] Handle the edge case where an elected worker is jailed (re-run VRF selection excluding jailed set)

Deliverable: Every block header contains a valid VRF proof; worker assignments are deterministic and verifiable.

---

### Phase 4 — Commit-reveal (MEV protection)

**Goal:** Proposer cannot read prompt content before sealing the batch.

Protocol:
1. Users submit `CommitTx { job_id, commitment = keccak256(prompt || nonce) }` in Phase 1 (0–1s)
2. Proposer seals `commitment_root = merkle(all commitments in mempool)` into the block header — proposer never sees plaintext prompts
3. Users reveal `RevealTx { job_id, prompt, nonce }` at the start of Phase 2; on-chain verification: `keccak256(prompt || nonce) == commitment`
4. Revealed prompts are forwarded to the elected worker validators for routing

Tasks:
- [ ] `CommitTx` transaction type + mempool handling
- [ ] `RevealTx` transaction type + on-chain commitment verification
- [ ] Punish reveals that don't match their commitment (fee burned, job dropped)
- [ ] Worker validators receive revealed prompts only after the commitment root is finalised on-chain

Deliverable: Proposer cannot front-run or reorder based on prompt content.

---

### Phase 5 — Model registry

**Goal:** On-chain registry of approved model versions; enables deterministic output verification.

Tasks:
- [ ] `ModelRegistry` state: `model_id → { weights_hash, tokenizer_hash, quantisation, status, approved_at_slot }`
- [ ] Governance `MsgProposeModel` / `MsgApproveModel`: simple 2/3 validator committee vote to add or deprecate a model
- [ ] Workers must serve only registered models; serving an unregistered model ID → commitment rejected
- [ ] Registry changelog: when a new model version is approved, the old version remains valid for a grace period (1 epoch) to allow validators to update their backends without being slashed
- [ ] `GetModel(model_id)` query for validator clients to verify they're running the correct version

Deliverable: Network agrees on exactly which model versions are canonical; validators serving wrong versions are detectable.

---

### Phase 6 — AI job pipeline (3-phase slot)

**Goal:** End-to-end job execution within a single 6-second slot.

**Phase 1 (0–1s): Commit**
- User submits `CommitTx` to mempool
- Proposer seals `commitment_root` into block header

**Phase 2 (1–4s): Route and respond**
- Users submit `RevealTx`; on-chain commitment verified
- Elected worker validators (3) forward job to their inference backends
- Each worker submits `OutputCommitment { output_hash = keccak256(output_tokens || job_id || validator_pubkey) }` on-chain before slot second 4 (commitment deadline)
- Late commitments (after 4s) are treated as missing → inactivity penalty

**Phase 3 (4–6s): BFT commit**
- 128-validator BFT committee checks:
  - All commitments are well-formed (valid BLS signature, valid job_id)
  - At least 2-of-3 worker commitments match (majority agreement)
- Block finalised via CometBFT prevote/precommit
- Mismatched commitment flagged → dispute window opened

Tasks:
- [ ] `JobRequest` transaction processing in DeliverTx
- [ ] `OutputCommitment` transaction + deadline enforcement in BeginBlock/EndBlock
- [ ] Majority-agreement check (2-of-3) in EndBlock; emit `DisputeOpen` event if mismatch
- [ ] Output delivery: once 2-of-3 agree, the agreed output hash is published; user can retrieve actual output off-chain from any of the agreeing validators
- [ ] Commitment deadline tracked per-slot; missing commitment increments validator's `consecutive_missed` counter

Deliverable: Jobs flow from user submission to on-chain commitments within a single slot.

---

### Phase 7 — Dispute and slashing

**Goal:** Dishonest commitments are detectable and penalised.

**Case A — Mismatched output hash (10% slash)**
- Triggered automatically when 1-of-3 worker commitments diverges from the majority in EndBlock
- 7-slot challenge window opens for the minority validator
- Validator may submit a `JustificationTx` (e.g. proving the model was updated mid-grace-period, verifiable against the registry changelog)
- If no valid justification submitted within 7 slots → 10% of bond slashed; jailed for 1 epoch

**Case B — User-submitted dispute (100% slash)**
- User received an output from a validator, but the validator's committed hash does not match
- User submits `DisputeTx { job_id, validator_pubkey, plaintext_output }`
- On-chain: recompute `keccak256(plaintext_output || job_id || validator_pubkey)` and compare to committed hash
- If mismatch: 100% bond slashed immediately; portion (e.g. 10%) sent to disputing user; validator permanently jailed
- If match: dispute rejected; user's dispute fee burned (anti-spam)

**Equivocation (100% slash)**
- Validator submits two different commitments for the same `job_id`
- Detected automatically by the committee in Phase 3
- Immediate 100% slash + permanent jail

Tasks:
- [ ] `DisputeOpen` state: track open disputes per `(job_id, validator_pubkey)` with expiry slot
- [ ] `JustificationTx` processing with registry changelog verification
- [ ] `DisputeTx` processing: on-chain hash recomputation + conditional slash
- [ ] Slashing logic: bond reduction, delegation haircut (pro-rata), jailing
- [ ] Equivocation detection on duplicate commitments per job_id
- [ ] Event emission for all slash events (for front-end / monitoring)

Deliverable: All three slashing cases are implemented and tested.

---

### Phase 8 — Inactivity leak

**Goal:** Validators who consistently miss commitments are drained and eventually ejected.

Rules (from spec):
- `consecutive_missed` counter incremented each slot a validator fails to submit commitment by deadline or is elected but absent
- After 4 consecutive epochs of inactivity (`consecutive_missed` reaches threshold): quadratic drain begins
- Drain rate: `penalty = bond * (consecutive_missed_since_threshold)^2 / K` per epoch (K = tuning constant)
- Once bond falls below 5,000 BIGT (min stake), validator is automatically jailed and removed from active set

Tasks:
- [ ] Track `consecutive_missed` and `last_active_epoch` per validator in state
- [ ] In EndBlock: update inactivity counters; apply quadratic drain for validators over threshold
- [ ] Automatic jailing when bond < min stake
- [ ] Reset `consecutive_missed` on any valid commitment submission
- [ ] Unjailing: validator must top up bond ≥ min stake and submit `MsgUnjail`

Deliverable: Long-offline validators lose their stake and are removed from the active set without manual intervention.

---

### Phase 9 — Reward distribution

**Goal:** Validators and delegators earn BIGT for honest service.

Design:
- Reward pool funded by: newly minted BIGT per epoch (inflation rate, governable) + job fees paid by users
- Per-epoch reward split: 80% to validators (proportional to jobs correctly served), 20% to delegators (proportional to stake)
- A validator's per-job reward = `base_reward_per_job * (1 - commission_rate)`; delegators receive `commission_rate` share
- Validators who had any slash event in the epoch receive reduced rewards (proportional to slash severity)

Tasks:
- [ ] Track `jobs_served` and `jobs_slashed` per validator per epoch
- [ ] In EndBlock at epoch boundary: compute each validator's reward share
- [ ] Distribute delegator rewards pro-rata against their stake snapshot at epoch start
- [ ] Commission rate: set by validator at registration, updateable with 1-epoch delay
- [ ] Emit `RewardDistributed` events for off-chain indexers

Deliverable: Honest validators and their delegators receive BIGT at the end of each epoch.

---

### Phase 10 — Validator router client

**Goal:** A standalone process (~800 lines) that any validator runs to participate in job serving.

Components (based on spec):

**listener/** (~50 lines)
```go
// Subscribe to mempool via JSON-RPC WebSocket
// Filter for JobRequest txs where elected_worker == self
// Extract: job_id, model_id, prompt (post-reveal), params
```

**router/** (~30 lines)
```go
// POST https://{backend}/v1/chat/completions
// Body: OpenAI-compatible JSON { model, messages, temperature: 0, ... }
// Backends: llama.cpp, Ollama, Together.ai, Fireworks.ai, Groq, RunPod, etc.
// Same code regardless of backend — only endpoint URL and API key differ
```

**broadcaster/** (~40 lines)
```go
// output_hash = keccak256(output_tokens || job_id || validator_pubkey)
// Sign with BLS key
// Submit OutputCommitment transaction via chain JSON-RPC
// Must complete before slot second 4 (commitment deadline)
```

**config.yaml** (non-code):
```yaml
chain_rpc: ws://localhost:26657/websocket
validator_key: /path/to/bls_key.json
backend:
  url: https://api.together.xyz/v1
  api_key: $TOGETHER_API_KEY
  model_id: meta-llama/Llama-3.3-70B-Instruct-Turbo
```

Tasks:
- [ ] Implement listener with WebSocket subscription to CometBFT mempool
- [ ] Implement OpenAI-compatible HTTP client with configurable backend URL + API key
- [ ] Implement BLS commitment broadcaster with slot-deadline awareness (drop job if < 1s left in slot)
- [ ] Retry logic for backend failures (single retry with 500ms timeout; give up and miss the slot rather than submit a late commitment)
- [ ] Config file loading (backend URL, API key, validator key path)
- [ ] Structured logging: job_id, backend latency, commitment submitted at slot-second X
- [ ] Binary builds for linux/amd64, linux/arm64, darwin/arm64

Deliverable: A validator can install this binary, configure their backend, and start serving jobs.

---

### Phase 11 — L1 checkpoint anchoring

**Goal:** Weak subjectivity checkpoints committed to an EVM L1 for long-range attack resistance.

Design:
- Every epoch, the BIGT chain produces a `Checkpoint { epoch, block_hash, validator_set_hash }`
- A permissioned set of relayers submits this checkpoint to `CheckpointAnchor.sol` on Ethereum (or another EVM chain)
- Light clients and new nodes can bootstrap from an L1-verified checkpoint rather than syncing from genesis
- The contract verifies a 2/3+ BLS multisig from the validator set recorded in the previous checkpoint

Tasks:
- [ ] `CheckpointAnchor.sol`:
  - `submitCheckpoint(epoch, blockHash, validatorSetHash, blsAggSig, validatorBitmap)`
  - Stores latest checkpoint; emits `CheckpointAnchored` event
  - Verifies BLS aggregate signature against the stored validator set
- [ ] BIGT chain: generate BLS aggregate checkpoint signature in EndBlock at epoch boundary
- [ ] Relayer service: watches for epoch-boundary blocks, submits checkpoint to L1
- [ ] Node bootstrap: accept `--trusted-checkpoint epoch=N,hash=0x...` flag instead of genesis sync

Deliverable: New nodes can sync from an L1-verified checkpoint; the system is resistant to long-range attacks.

---

### Phase 12 — Testing and hardening

Tasks:
- [ ] Unit tests: VRF election, commitment hash, slashing logic, inactivity drain formula, reward distribution math
- [ ] Integration tests: full single-slot lifecycle (job submit → commit-reveal → 3 workers route → commitments → BFT finalize)
- [ ] Dispute integration test: inject a bad commitment; verify 10% slash applied; verify user dispute triggers 100% slash
- [ ] Inactivity integration test: simulate 4+ epochs of missed commitments; verify quadratic drain and auto-jail
- [ ] Load test: 100 concurrent job requests per slot; verify commitment deadline is met under load
- [ ] Testnet: 4-node devnet with one validator using Together.ai backend; run for 24h (1 epoch); verify reward distribution
- [ ] Security review: audit slashing conditions for griefing vectors (e.g. validator submits dispute against themselves to trigger early unbonding — verify this is not possible)

---

## Key parameters (from spec)

| Parameter | Value |
|---|---|
| Slot time | 6 seconds |
| Commitment deadline | 4 seconds into slot |
| Epoch length | 14,400 slots (~24h) |
| Min stake | 5,000 BIGT |
| Validators per job | 3 (VRF elected) |
| BFT committee size | 128 validators |
| Dispute window | 7 slots (~42s) |
| Slash: provably malicious | 100% of bond |
| Slash: unmatched (disputed not proven) | 10% of bond |
| Inactivity threshold | 4 consecutive epochs |
| Max validator stake share | 5% of total |
| Unbonding period | 21 days |

---

## Dependency notes

- **CometBFT v0.38.x** — BFT consensus engine (drop-in replacement for Tendermint)
- **Cosmos SDK v0.50.x** — optional; can use raw ABCI if a lighter footprint is preferred
- **BLS12-381** — validator keys (Herumi/bls-eth-go or gnark-crypto)
- **OpenAI Go SDK or raw net/http** — inference router (no inference library needed)
- **Solidity 0.8.x + Foundry** — L1 checkpoint contract
- **CometBFT VRF** — use ECVRF (IETF draft) with validator's ed25519 consensus key

---

## Implementation order rationale

Phases 1–3 are blockers for everything else (no chain, no staking, no validator set).
Phase 4 (commit-reveal) and Phase 5 (model registry) can be developed in parallel after Phase 2.
Phase 6 (job pipeline) requires Phases 4 and 5 to be complete.
Phases 7–9 (dispute, inactivity, rewards) require Phase 6.
Phase 10 (validator client) can be developed in parallel with Phases 6–9 and integrated at Phase 6 completion.
Phase 11 (L1 anchoring) is independent and can be developed in parallel from Phase 3 onward.
Phase 12 (testing) is ongoing throughout but the full integration suite requires Phase 9 to be complete.
