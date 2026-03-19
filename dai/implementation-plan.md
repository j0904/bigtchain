# BIGT — Implementation Plan (Single-Server with Observer Consensus)

Source spec: [claude.md](claude.md)

---

## Architecture overview

**Core design change:** Only **one VRF-elected validator** runs the AI inference service per slot. All other validators in the active set act as **observers** — they subscribe to on-chain events, record the user's prompt and the serving validator's response, and can later attest or dispute the commitment. This eliminates redundant inference calls (saving 2/3 of compute costs) while preserving economic security through event-driven protocolling and user-submitted disputes.

**How it works:**
1. VRF elects a single **serving validator** per slot.
2. The serving validator routes the prompt to its AI backend, returns output to the user, and commits the output hash on-chain.
3. A **PromptRevealed** event and a **ResponseCommitted** event are emitted on-chain for every job.
4. All other validators (**observer validators**) subscribe to these events, independently record (prompt, response_hash, job_id, serving_validator) in their local state, and can re-execute the inference off-chain at any time to verify correctness.
5. If an observer or user detects a mismatch, they file a dispute. Disputes trigger a **consensus vote** among validators — if 2/3+ of the active set votes to uphold the dispute, the serving validator's block is **reverted** and the validator is slashed.
6. The serving validator **shares job rewards** with all observer validators as compensation for their protocolling and verification work.

This is an **optimistic single-server model with consensus-backed reversion** — the serving validator is trusted by default, but any user or observer can challenge a commitment and force a validator vote to revert the block and slash the dishonest server.

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
│   │   ├── vrf/         # VRF-based single worker election per slot
│   │   ├── commitreveal/# Commit-reveal for MEV protection
│   │   ├── jobs/        # AI job mempool, single-server slot pipeline
│   │   ├── events/      # Event definitions for prompt/response protocolling
│   │   ├── dispute/     # Challenge window, observer + user proofs
│   │   └── rewards/     # Per-epoch BIGT reward distribution
│   ├── registry/        # ModelRegistry (weights hash, governance approval)
│   └── genesis/         # Genesis file, initial validator set, token supply
├── validator/           # Validator client (Go)
│   ├── listener/        # Chain listener: event subscription via JSON-RPC
│   ├── router/          # Inference router: OpenAI-compatible HTTP client (serving mode)
│   ├── observer/        # Event recorder + optional re-execution verifier (observer mode)
│   ├── voter/           # Dispute vote caster: subscribe to DisputeOpened, cast DisputeVoteTx
│   └── broadcaster/     # Commitment/dispute broadcaster: BLS sign + submit tx
├── contracts/           # Solidity contracts for L1 checkpoint anchoring
│   └── CheckpointAnchor.sol
├── proto/               # Protobuf definitions (job request, commitment, events, dispute)
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
  - `DisputeTx { job_id, validator_pubkey, plaintext_output, disputer_type (user|observer) }`
- [ ] Define event types:
  - `PromptRevealed { job_id, model_id, prompt_hash, user_address, slot }`
  - `ResponseCommitted { job_id, serving_validator, output_hash, slot, signature }`
  - `DisputeOpened { job_id, serving_validator, disputer, dispute_type, slot }`
  - `DisputeVoteCast { job_id, voter_pubkey, vote (uphold|dismiss), slot }`
  - `BlockReverted { slot, serving_validator, reason, dispute_id }`
  - `RewardShared { job_id, serving_validator, observer_validators[], serving_share, observer_share }`
- [ ] Single-node devnet boots and produces blocks

Deliverable: `chain/` compiles, single-node devnet produces 6-second blocks with event emission infrastructure.

---

### Phase 2 — Staking module

**Goal:** Validators can register, bond BIGT, and be tracked for the active set.

Tasks:
- [ ] `MsgRegisterValidator`: pubkey (BLS + consensus), bond amount ≥ 5,000 BIGT, router endpoint metadata, `role_capability` flag (serving-capable or observer-only)
- [ ] `MsgDelegate` / `MsgUndelegate`: delegator bonding; 21-day unbonding queue processed in EndBlock
- [ ] Enforce 5% cap: no single validator may hold > 5% of total stake
- [ ] KES (key-evolving signature) key rotation: validators submit new forward-secret keys each epoch; old keys rejected after the epoch boundary
- [ ] `ValidatorSet` state: active / jailed / unbonding status
- [ ] Epoch boundary (every 14,400 slots ≈ 24h): snapshot active set, compute voting power

Deliverable: Validators can join/leave the active set; stake snapshots are correct at epoch boundaries.

---

### Phase 3 — VRF slot election (single server)

**Goal:** Each slot deterministically elects 1 proposer + **1 serving validator** using VRF.

Tasks:
- [ ] Each validator in the active set computes `VRF_prove(slot_seed, validator_sk)` off-chain and includes the VRF proof in their block proposal
- [ ] `slot_seed = keccak256(epoch_seed || slot_number)` where `epoch_seed` is committed at epoch start
- [ ] Sort serving-capable validators by `VRF_hash(proof)` mod stake; lowest hash wins the **serving slot**
- [ ] On-chain `VRF_verify` in BeginBlock: reject block if proposer proof invalid
- [ ] **Single serving validator** assignment published in BeginBlock; all other active validators automatically become observers for this slot
- [ ] Handle the edge case where the elected server is jailed (re-run VRF selection excluding jailed set)
- [ ] Emit `ServerElected { slot, validator_pubkey }` event so all observers know who is serving

Deliverable: Every block header contains a valid VRF proof; exactly one serving validator is elected per slot; all others are observers.

---

### Phase 4 — Commit-reveal (MEV protection)

**Goal:** Proposer cannot read prompt content before sealing the batch.

Protocol:
1. Users submit `CommitTx { job_id, commitment = keccak256(prompt || nonce) }` in Phase 1 (0–1s)
2. Proposer seals `commitment_root = merkle(all commitments in mempool)` into the block header — proposer never sees plaintext prompts
3. Users reveal `RevealTx { job_id, prompt, nonce }` at the start of Phase 2; on-chain verification: `keccak256(prompt || nonce) == commitment`
4. Revealed prompt is forwarded to the **single elected serving validator** for routing
5. On reveal, emit `PromptRevealed { job_id, model_id, prompt_hash, user_address, slot }` — all observer validators receive this event

Tasks:
- [ ] `CommitTx` transaction type + mempool handling
- [ ] `RevealTx` transaction type + on-chain commitment verification
- [ ] Punish reveals that don't match their commitment (fee burned, job dropped)
- [ ] Emit `PromptRevealed` event on successful reveal for observer validators to record
- [ ] Serving validator receives revealed prompt only after the commitment root is finalised on-chain

Deliverable: Proposer cannot front-run or reorder based on prompt content; all observers are notified of every prompt via events.

---

### Phase 5 — Model registry

**Goal:** On-chain registry of approved model versions; enables deterministic output verification.

Tasks:
- [ ] `ModelRegistry` state: `model_id → { weights_hash, tokenizer_hash, quantisation, status, approved_at_slot }`
- [ ] Governance `MsgProposeModel` / `MsgApproveModel`: simple 2/3 validator committee vote to add or deprecate a model
- [ ] Serving validator must serve only registered models; serving an unregistered model ID → commitment rejected
- [ ] Registry changelog: when a new model version is approved, the old version remains valid for a grace period (1 epoch) to allow validators to update their backends without being slashed
- [ ] `GetModel(model_id)` query for validator clients to verify they're running the correct version

Deliverable: Network agrees on exactly which model versions are canonical; validators serving wrong versions are detectable.

---

### Phase 6 — AI job pipeline (single-server slot)

**Goal:** End-to-end job execution within a single 6-second slot, served by one validator, protocolled by all others.

**Phase 1 (0–1s): Commit**
- User submits `CommitTx` to mempool
- Proposer seals `commitment_root` into block header

**Phase 2 (1–4s): Route and respond (single server)**
- User submits `RevealTx`; on-chain commitment verified; `PromptRevealed` event emitted
- The **single elected serving validator** forwards the job to its inference backend
- Serving validator submits `OutputCommitment { output_hash = keccak256(output_tokens || job_id || validator_pubkey) }` on-chain before slot second 4 (commitment deadline)
- Late commitment (after 4s) is treated as missing → inactivity penalty; job is re-queued for next slot
- On commitment, emit `ResponseCommitted { job_id, serving_validator, output_hash, slot, signature }`

**Phase 3 (4–6s): BFT commit + observer protocolling**
- BFT committee checks:
  - Commitment is well-formed (valid BLS signature, valid job_id, serving validator matches VRF election)
  - Serving validator is the elected server for this slot (reject commitments from non-elected validators)
- Block finalised via CometBFT prevote/precommit
- **Observer validators** receive both `PromptRevealed` and `ResponseCommitted` events — they record the tuple `(job_id, prompt_hash, output_hash, serving_validator, slot)` in their local protocol log
- Observers **may** optionally re-execute the inference against the same registered model and compare output hashes; mismatches trigger an observer dispute

**Post-slot: Dispute window (7 slots + 3 slots vote)**
- If a user or observer detects a problem, they file a dispute within 7 slots
- The serving validator has the dispute window to submit a `JustificationTx`
- If no valid justification: a **consensus vote** is triggered (3-slot vote window)
- Each active validator casts a `DisputeVoteTx` (uphold or dismiss)
- If 2/3+ of active stake votes to uphold: the block is **reverted**, the serving validator is slashed, and affected jobs are re-queued to the next slot
- Cryptographically provable fraud (hash mismatch, equivocation) triggers **immediate** reversion without a vote

Tasks:
- [ ] `JobRequest` transaction processing in DeliverTx
- [ ] `OutputCommitment` transaction + deadline enforcement (single commitment per job, from elected server only)
- [ ] Emit `ResponseCommitted` event in EndBlock after commitment is validated
- [ ] Reject commitments from validators who were not VRF-elected for the slot
- [ ] Job re-queue logic: if serving validator misses the deadline, job moves to next slot's queue
- [ ] Commitment deadline tracked per-slot; missing commitment increments serving validator's `consecutive_missed` counter

Deliverable: Jobs flow from user submission to single-server commitment within a slot; all validators are notified via events.

---

### Phase 7 — Event-driven observer protocol

**Goal:** Observer validators record all prompts and responses, enabling distributed verification and dispute filing.

**Event subscription model:**
- Every validator runs an **event listener** that subscribes to `PromptRevealed` and `ResponseCommitted` events via CometBFT WebSocket/JSON-RPC
- Events are indexed locally by `(job_id, slot)` for fast lookup

**Observer protocol log:**
- Each observer maintains a local append-only log: `{ job_id, prompt_hash, output_hash, serving_validator, slot, timestamp }`
- Log entries are signed by the observer's BLS key for non-repudiation
- Log is prunable after the dispute window expires (7 slots) unless a dispute is open

**Optional re-execution verification:**
- Observers can run inference against the same registered model (same weights_hash, tokenizer_hash, temperature=0) to independently verify the output
- If the observer's output hash differs from the serving validator's committed hash, the observer submits an `ObserverDisputeTx`
- Re-execution is **optional** — observers are not penalised for not re-executing, but they earn bonus rewards for successfully catching dishonest servers (see Phase 9)

**Protocol attestation:**
- At epoch boundary, each observer submits a signed `ProtocolAttestation { epoch, jobs_observed, jobs_verified, observer_pubkey }`
- This attestation proves the observer was online and recording events; used for reward eligibility

Tasks:
- [ ] Define `ProtocolLog` local storage schema (append-only, indexed by job_id + slot)
- [ ] Event subscription handler for `PromptRevealed` and `ResponseCommitted`
- [ ] `ObserverDisputeTx` transaction type: observer submits `{ job_id, observer_output_hash, observer_pubkey, observer_signature }`
- [ ] On-chain verification: compare `observer_output_hash` to serving validator's committed hash; if mismatch, open dispute
- [ ] `ProtocolAttestation` transaction processing in EndBlock at epoch boundary
- [ ] Emit `ObserverAttested { observer_pubkey, epoch, jobs_observed }` event

Deliverable: All validators maintain a verifiable audit trail of prompts and responses; observers can independently catch dishonest serving validators.

---

### Phase 8 — Dispute, consensus vote, and block reversion

**Goal:** Users and observer validators can raise disputes that trigger a consensus vote to revert the serving validator's block and slash their bond.

**Dispute flow (all cases):**
1. A dispute is filed (by user or observer)
2. A **7-slot dispute window** opens
3. The serving validator may submit a `JustificationTx` within the window
4. If no justification, or justification is rejected → a **consensus vote** is triggered among all active validators
5. Each validator casts `DisputeVoteTx { job_id, vote: uphold | dismiss }`
6. If **2/3+ of active validators** vote to uphold → the block containing the disputed commitment is **reverted**, the serving validator is slashed, and affected jobs are re-queued
7. If < 2/3 vote to uphold → dispute dismissed

**Case A — Observer-detected mismatch (10% slash + block reversion)**
- Triggered when an observer re-executes inference and their output hash differs from the serving validator's commitment
- Observer submits `ObserverDisputeTx { job_id, observer_output_hash }`
- If consensus vote upholds: block reverted, 10% of bond slashed, jailed for 1 epoch
- Observer who raised the dispute receives bounty from slashed bond

**Case B — User-submitted dispute (100% slash + block reversion)**
- User received an output from the serving validator, but the validator's committed hash does not match
- User submits `DisputeTx { job_id, validator_pubkey, plaintext_output }`
- On-chain: recompute `keccak256(plaintext_output || job_id || validator_pubkey)` and compare to committed hash
- If hash mismatch is cryptographically provable: **immediate** 100% slash + block reversion (no vote needed — the proof is deterministic)
- If hash matches but user claims output quality issue: consensus vote required (2/3+ to uphold)
- Portion (10%) of slashed bond sent to disputing user; validator permanently jailed

**Case C — Equivocation (100% slash + block reversion)**
- Serving validator submits two different commitments for the same `job_id`
- Detected automatically by the committee in Phase 3
- Immediate 100% slash + permanent jail + block reversion (no vote needed — equivocation is self-evident)

**Block reversion mechanics:**
- When a block is reverted, all jobs in that block's commitment set are moved back to the job queue
- Reverted jobs are re-assigned to the next slot's VRF-elected serving validator
- Users who submitted jobs in the reverted block are not charged fees
- The reverted block's state changes are rolled back; a `BlockReverted` event is emitted
- Chain state is reconstructed from the last finalized non-reverted block

Tasks:
- [ ] `ObserverDisputeTx` processing: compare observer re-execution hash vs committed hash; open dispute window
- [ ] `DisputeTx` processing: on-chain hash recomputation; immediate slash if cryptographically provable, otherwise open vote
- [ ] `DisputeVoteTx` transaction type: validator casts uphold/dismiss vote within vote window (3 slots after dispute window)
- [ ] Vote tallying in EndBlock: count votes weighted by stake; 2/3+ threshold to uphold
- [ ] **Block reversion logic:** roll back state changes from disputed block; re-queue affected jobs; emit `BlockReverted` event
- [ ] `DisputeOpen` state: track open disputes per `(job_id, serving_validator)` with expiry slot and vote tally
- [ ] `JustificationTx` processing with registry changelog verification
- [ ] Slashing logic: bond reduction, delegation haircut (pro-rata), jailing
- [ ] Equivocation detection on duplicate commitments per job_id (auto-revert, no vote)
- [ ] Reward disputer (user or observer) who successfully triggers a slash (bounty from slashed bond)
- [ ] Event emission: `DisputeOpened`, `DisputeVoteCast`, `BlockReverted`, `ValidatorSlashed`, `DisputeResolved`
- [ ] Anti-spam: dispute filing requires a deposit; returned if dispute upheld, burned if dismissed

Deliverable: Users and observers can dispute commitments; consensus vote triggers block reversion and slashing; all three dispute cases are implemented and tested.

---

### Phase 9 — Inactivity leak

**Goal:** Validators who consistently miss commitments or observations are drained and eventually ejected.

Rules:
- **Serving validator inactivity:** `consecutive_missed` counter incremented each slot the elected serving validator fails to submit commitment by deadline
- **Observer inactivity:** observers who fail to submit `ProtocolAttestation` at epoch boundary are marked inactive; they still earn base staking rewards but lose observer bonus rewards
- After 4 consecutive epochs of serving inactivity: quadratic drain begins
- Drain rate: `penalty = bond * (consecutive_missed_since_threshold)^2 / K` per epoch (K = tuning constant)
- Once bond falls below 5,000 BIGT (min stake), validator is automatically jailed and removed from active set

Tasks:
- [ ] Track `consecutive_missed` and `last_active_epoch` per validator in state
- [ ] Track `epochs_attested` per observer validator
- [ ] In EndBlock: update inactivity counters; apply quadratic drain for validators over threshold
- [ ] Automatic jailing when bond < min stake
- [ ] Reset `consecutive_missed` on any valid commitment submission
- [ ] Unjailing: validator must top up bond ≥ min stake and submit `MsgUnjail`

Deliverable: Long-offline validators lose their stake and are removed from the active set without manual intervention.

---

### Phase 10 — Reward distribution (revenue sharing)

**Goal:** The serving validator shares job revenue with all observer validators who protocolled the slot, incentivising both service and verification.

**Revenue-sharing model:**
- Each job generates a **job fee** (paid by the user) + a **block reward** (newly minted BIGT per slot)
- The serving validator earns the job revenue but **shares it** with observer validators as compensation for their protocolling and verification work
- This creates a cooperative incentive: observers are motivated to watch honestly because they earn a share, and the serving validator benefits from observers' attestation which increases trust in the network

**Per-job reward split:**
- **Serving validator: 50%** of (job fee + block reward) — the server did the inference work
- **Observer pool: 30%** of (job fee + block reward) — split equally among all observers who submitted `ProtocolAttestation` for this epoch
- **Delegator pool: 15%** of (job fee + block reward) — distributed proportional to stake
- **Dispute bounty reserve: 5%** — accumulated into a pool; paid out to users or observers who successfully file disputes that result in slashing

**Per-epoch distribution:**
- At epoch boundary, aggregate all per-job shares into validator accounts
- Serving validators earn 50% × (total jobs they served)
- Observer validators earn an equal share of 30% × (total jobs in the epoch) — requires valid `ProtocolAttestation`
- Delegators earn 15% proportional to their stake snapshot at epoch start
- Dispute bounty pool rolls over if unused; paid immediately on successful dispute resolution
- Validators who had any slash event or reverted block in the epoch receive **zero rewards** for the epoch; their share is redistributed to the observer pool

**Commission:**
- A validator's reward (serving or observer) = `gross_reward * (1 - commission_rate)`; the `commission_rate` portion goes to the validator's delegators
- Commission rate set at registration, updateable with 1-epoch delay

Tasks:
- [ ] Track `jobs_served`, `jobs_observed`, `disputes_won`, and `blocks_reverted` per validator per epoch
- [ ] Implement per-job revenue split: 50/30/15/5 between server/observers/delegators/bounty
- [ ] In EndBlock at epoch boundary: aggregate per-job shares; compute each validator's total reward
- [ ] Distribute observer rewards equally among all validators with valid `ProtocolAttestation` for the epoch
- [ ] Redistribute slashed/reverted validator's epoch rewards to observer pool
- [ ] Distribute delegator rewards pro-rata against their stake snapshot at epoch start
- [ ] Dispute bounty: pay out immediately on dispute resolution; accumulate unused bounty across epochs
- [ ] Commission rate: set by validator at registration, updateable with 1-epoch delay
- [ ] Emit `RewardShared { job_id, serving_validator, observer_count, serving_share, observer_share }` per job
- [ ] Emit `RewardDistributed` events per validator at epoch boundary for off-chain indexers

Deliverable: Serving validators share revenue with observer validators; all participants are compensated fairly for their role; dishonest validators forfeit their epoch rewards.

---

### Phase 11 — Validator client (dual-mode)

**Goal:** A standalone process (~600–1000 lines) that runs in **serving mode** when VRF-elected, **observer mode** otherwise, and **votes on disputes** when they arise.

**Serving mode** (when VRF-elected for the slot):

**listener/** (~50 lines)
```go
// Subscribe to mempool via JSON-RPC WebSocket
// Filter for JobRequest txs where elected_server == self
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

**Observer mode** (all other slots — three sub-components):

**observer/subscriber** (~40 lines)
```go
// Subscribe to PromptRevealed and ResponseCommitted events via CometBFT WebSocket
// Record (job_id, prompt_hash, output_hash, serving_validator, slot) in local protocol log
// Append-only log, indexed by job_id + slot, signed by observer's BLS key
// Prunable after dispute window expires (7 slots) unless a dispute is open
```

**observer/verifier** (~30 lines)
```go
// If verify_enabled: re-execute inference against same registered model (temperature=0)
// Compare observer's output hash to serving validator's committed hash
// If mismatch detected: submit ObserverDisputeTx via broadcaster
// Optional — not penalised for skipping, but earns bounty for catching dishonest servers
```

**observer/attestor** (~10 lines)
```go
// At epoch boundary: submit signed ProtocolAttestation { epoch, jobs_observed, jobs_verified }
// Required for observer reward eligibility (30% pool)
// Proves the observer was online and recording events throughout the epoch
```

**Dispute voting** (active during open disputes):

**voter/** (~30 lines)
```go
// Subscribe to DisputeOpened events via WebSocket
// When dispute enters vote phase (after 7-slot justification window):
//   - If verify_enabled: check local protocol log + re-execution result
//   - Cast DisputeVoteTx { job_id, vote: uphold | dismiss } via broadcaster
//   - Vote must be cast within 3-slot vote window
// Auto-vote policy configurable: auto_uphold_on_mismatch, manual, abstain
```

**config.yaml** (non-code):
```yaml
chain_rpc: ws://localhost:26657/websocket
validator_key: /path/to/bls_key.json
observer:
  verify_enabled: true           # enable re-execution verification
  protocol_log_path: /data/protocol.log
dispute:
  auto_vote: true                # auto-vote on disputes based on re-execution
  vote_policy: uphold_on_mismatch  # uphold_on_mismatch | always_dismiss | manual
backend:
  url: https://api.together.xyz/v1
  api_key: $TOGETHER_API_KEY
  model_id: meta-llama/Llama-3.3-70B-Instruct-Turbo
```

Tasks:
- [ ] Implement dual-mode switching: detect VRF election result in BeginBlock event; switch to serving or observer mode
- [ ] Implement serving-mode listener with WebSocket subscription to CometBFT mempool
- [ ] Implement OpenAI-compatible HTTP client with configurable backend URL + API key
- [ ] Implement BLS commitment broadcaster with slot-deadline awareness (drop job if < 1s left in slot)
- [ ] Implement observer event subscriber (~40 lines): subscribe to `PromptRevealed` and `ResponseCommitted`, write to protocol log
- [ ] Implement observer re-execution verifier (~30 lines): configurable via `verify_enabled`, compare output hashes, trigger `ObserverDisputeTx` on mismatch
- [ ] Implement observer epoch attestor (~10 lines): submit `ProtocolAttestation` at epoch boundary
- [ ] Implement dispute voter (~30 lines): subscribe to `DisputeOpened` events, cast `DisputeVoteTx` within vote window based on local evidence and vote policy
- [ ] Retry logic for backend failures (single retry with 500ms timeout; give up and miss the slot rather than submit a late commitment)
- [ ] Config file loading (backend URL, API key, validator key path, observer settings, dispute vote policy)
- [ ] Structured logging: job_id, mode (serving/observer/voting), backend latency, commitment submitted at slot-second X
- [ ] Binary builds for linux/amd64, linux/arm64, darwin/arm64

Deliverable: A validator can install this binary and automatically serve inference when elected, observe and protocol events otherwise, and vote on disputes when they arise.

---

### Phase 12 — L1 checkpoint anchoring

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

### Phase 13 — Testing and hardening

Tasks:
- [ ] Unit tests: VRF single-server election, commitment hash, slashing logic, inactivity drain formula, reward distribution math, observer attestation
- [ ] Integration tests: full single-slot lifecycle (job submit → commit-reveal → 1 server routes → commitment → observers record events → BFT finalize)
- [ ] Observer dispute test: inject a bad commitment from serving validator; verify observer detects mismatch via re-execution; verify consensus vote triggers; verify 10% slash + block reversion applied
- [ ] User dispute test: verify user dispute with cryptographic proof triggers immediate 100% slash + block reversion (no vote)
- [ ] Consensus vote test: simulate dispute vote with varying validator participation; verify 2/3+ threshold required; verify block reversion only on upheld vote
- [ ] Block reversion test: verify state rollback is correct after reversion; verify re-queued jobs are served in next slot; verify user is not charged for reverted jobs
- [ ] Revenue sharing test: verify serving validator receives 50%, observers receive 30% split equally, delegators receive 15%; verify slashed validator's rewards redistributed
- [ ] Event protocolling test: verify all observers receive `PromptRevealed` and `ResponseCommitted` events; verify `ProtocolAttestation` is correctly submitted at epoch boundary
- [ ] Inactivity integration test: simulate 4+ epochs of missed commitments; verify quadratic drain and auto-jail
- [ ] Load test: 100 concurrent job requests per slot; verify commitment deadline is met under load
- [ ] Testnet: 4-node devnet (1 serving validator rotating via VRF, 3 observers) using Together.ai backend; run for 24h (1 epoch); verify revenue sharing across serving + observer pools
- [ ] Reversion testnet scenario: deliberately inject bad commitment; verify dispute → consensus vote → block reversion → job re-queue → re-serve in next slot
- [ ] Security review: audit slashing conditions for griefing vectors; verify observer cannot grief serving validator with false disputes (observer must submit verifiable re-execution output); verify block reversion cannot be weaponised to DoS the network (dispute deposit + anti-spam)

---

## Key parameters

| Parameter | Value | Notes |
|---|---|---|
| Slot time | 6 seconds | |
| Commitment deadline | 4 seconds into slot | |
| Epoch length | 14,400 slots (~24h) | |
| Min stake | 5,000 BIGT | |
| Serving validators per job | **1** (VRF elected) | Changed from 3 |
| Observer validators per job | **All other active validators** | New: event-driven protocolling |
| BFT committee size | 128 validators | |
| Dispute window | 7 slots (~42s) | |
| Slash: provably malicious (user dispute) | 100% of bond + block reversion | Immediate, no vote needed |
| Slash: observer-detected mismatch | 10% of bond + block reversion | Requires 2/3+ consensus vote |
| Slash: equivocation | 100% of bond + block reversion | Immediate, no vote needed |
| Dispute vote threshold | 2/3+ of active validator stake | Weighted by stake |
| Dispute vote window | 3 slots after dispute window | ~18s to cast votes |
| Inactivity threshold | 4 consecutive epochs | |
| Max validator stake share | 5% of total | |
| Unbonding period | 21 days | |
| Serving validator share | **50%** of job revenue | Revenue sharing |
| Observer pool share | **30%** of job revenue | Split equally among attestors |
| Delegator pool share | **15%** of job revenue | Proportional to stake |
| Dispute bounty reserve | **5%** of job revenue | Paid on successful disputes |

---

## Dependency notes

- **CometBFT v0.38.x** — BFT consensus engine (drop-in replacement for Tendermint)
- **Cosmos SDK v0.50.x** — optional; can use raw ABCI if a lighter footprint is preferred
- **BLS12-381** — validator keys (Herumi/bls-eth-go or gnark-crypto)
- **OpenAI Go SDK or raw net/http** — inference router (no inference library needed)
- **Solidity 0.8.x + Foundry** — L1 checkpoint contract
- **CometBFT VRF** — use ECVRF (IETF draft) with validator's ed25519 consensus key
- **CometBFT Event Subscription** — WebSocket/JSON-RPC event queries for observer protocolling

---

## Implementation order rationale

Phases 1–3 are blockers for everything else (no chain, no staking, no single-server election).
Phase 4 (commit-reveal) and Phase 5 (model registry) can be developed in parallel after Phase 2.
Phase 6 (single-server job pipeline) requires Phases 4 and 5 to be complete.
Phase 7 (observer event protocol) can be developed in parallel with Phase 6 — it depends on event types defined in Phase 1 but not on the full job pipeline.
Phases 8–10 (dispute, inactivity, rewards) require Phases 6 and 7.
Phase 11 (validator client) can be developed in parallel with Phases 6–10 and integrated at Phase 7 completion.
Phase 12 (L1 anchoring) is independent and can be developed in parallel from Phase 3 onward.
Phase 13 (testing) is ongoing throughout but the full integration suite requires Phase 10 to be complete.
