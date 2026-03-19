# BIGT — Proof of Useful Stake (Single-Server with Observer Consensus)

The core design: **one VRF-elected validator** serves AI inference per slot, while **all other validators observe and protocol** the prompts and responses via on-chain events. The serving validator routes the job, commits the output hash, and shares the job revenue with observers. If the output is dishonest, any user or observer can raise a dispute — and if 2/3+ of validators vote to uphold it, the block is **reverted** and the serving validator is slashed.

This is an optimistic single-server model with consensus-backed reversion. Only one validator needs to run inference per job (eliminating redundant compute costs), while the full validator set provides economic security through event-driven protocolling and dispute consensus.

---

## What changes and why

In the previous design, three validators independently ran inference for every job (redundant multi-validator execution). This tripled compute costs and required all three validators to maintain inference backends at all times. The revised model separates three roles:

- **Serving validator** (1 per slot) — routes the prompt to an AI backend, returns the output, commits the hash on-chain, and shares revenue with observers
- **Observer validators** (all others) — subscribe to on-chain events, record every prompt and response, and can optionally re-execute inference to verify correctness
- **Disputers** (users or observers) — file disputes that trigger a consensus vote among all validators to revert the block

Only one validator runs inference per job. The others watch and verify. If the serving validator is dishonest, any participant can challenge it and the full validator set votes on whether to revert the block. This is the same trust model used by optimistic rollups — assume honest execution by default, with a fraud-proof mechanism backed by economic stake.

---

## How the single-server model works

Each slot, VRF elects exactly **one serving validator**. That validator runs a lightweight **router node** — a small Go process that does the following:

1. Detects that it has been VRF-elected for the slot (via `ServerElected` event)
2. Receives AI job requests from the mempool (prompts revealed after commit-reveal)
3. Forwards each request to its inference backend over HTTP
4. Receives the output tokens
5. Hashes the output and signs the commitment with its BLS key
6. Broadcasts the signed `OutputCommitment` on-chain before the 4-second deadline
7. Shares the job revenue with observer validators

All other validators operate in **observer mode** for that slot:

1. Subscribe to `PromptRevealed` and `ResponseCommitted` events via CometBFT WebSocket
2. Record each `(job_id, prompt_hash, output_hash, serving_validator, slot)` in a local protocol log
3. Optionally re-execute inference against the same registered model to verify the output
4. If a mismatch is detected, submit an `ObserverDisputeTx` to trigger a consensus vote
5. At epoch boundary, submit a signed `ProtocolAttestation` to prove they were online and watching

The inference backend for the serving validator can be any of the following:

- A local GPU running llama.cpp or Ollama
- A remote GPU rented from RunPod, Lambda Labs, or Vast.ai
- A commercial API such as Together.ai, Fireworks.ai, or Groq
- A self-hosted cluster owned by the validator
- A decentralised compute network such as Akash or io.net

The protocol is agnostic to all of these. The validator's bond is what makes them economically accountable regardless of which backend they use.

---

## Verifiability: observer protocolling and consensus-backed disputes

With only one validator serving each job, verifiability shifts from redundant execution to **observer protocolling and dispute consensus**. Four mechanisms provide security:

**Event-driven observer protocolling.** Every prompt and response is recorded on-chain as events (`PromptRevealed`, `ResponseCommitted`). All observer validators subscribe to these events and maintain a local, signed, append-only protocol log. This creates a distributed audit trail — even if the serving validator is dishonest, every other validator has a record of what was committed.

**Deterministic model outputs.** Open models with temperature=0 (greedy decoding) are deterministic given the same weights and tokenizer. The protocol registers specific quantized model versions in the ModelRegistry (by weights hash). Observers who re-execute inference against the same registered model can independently verify that the serving validator's output hash is correct. Any divergence is suspicious and triggers the dispute process.

**Consensus-backed dispute and block reversion.** When a dispute is filed (by a user or an observer), the following process runs:

1. A **7-slot dispute window** opens for the serving validator to submit a `JustificationTx`
2. If no valid justification is submitted, a **consensus vote** is triggered among all active validators
3. Each validator casts a `DisputeVoteTx` (uphold or dismiss) within a 3-slot vote window
4. If **2/3+ of active stake** votes to uphold, the serving validator's block is **reverted** — all state changes rolled back, affected jobs re-queued to the next slot, and the serving validator is slashed
5. If < 2/3 vote to uphold, the dispute is dismissed and the disputer's deposit is burned

Cryptographically provable fraud (hash mismatch, equivocation) triggers **immediate** reversion and slashing without requiring a vote — the proof is deterministic and self-evident.

**User-submitted disputes.** A user who receives an output that does not match the on-chain commitment hash can submit a dispute transaction containing the plaintext output. The hash is recomputed on-chain and compared to the commitment. If they don't match, the block is reverted immediately, the validator's full bond is slashed, and the user receives a portion (10%) of the slashed bond as compensation.

**Observer-submitted disputes.** An observer who re-executes inference and finds a different output hash can submit an `ObserverDisputeTx`. This opens the dispute window and, if upheld by consensus vote, results in 10% bond slashing + block reversion. The observer receives a bounty from the slashed bond.

---

## The dual-mode validator node in practice

Every validator runs a single binary that operates in two modes, switching automatically each slot based on VRF election:

### Serving mode (when VRF-elected)

**1. Chain listener.** Watches the mempool for AI job requests addressed to this validator's slot. When a job arrives, it extracts the prompt, model ID, and parameters. Standard JSON-RPC subscription, about 50 lines of code.

**2. Inference router.** Sends the job to the configured backend. The recommended interface is the OpenAI-compatible REST API, which is supported by llama.cpp server, Ollama, Together.ai, Fireworks.ai, Groq, and most other inference providers. About 30 lines of HTTP client code.

**3. Commitment broadcaster.** Computes `keccak256(output_tokens || job_id || validator_pubkey)`, signs the hash with the validator's BLS key, and submits the commitment transaction on-chain. Must complete before slot second 4. About 40 lines.

### Observer mode (all other slots)

**4. Event subscriber.** Subscribes to `PromptRevealed` and `ResponseCommitted` events via CometBFT WebSocket. Records each `(job_id, prompt_hash, output_hash, serving_validator, slot)` in a local append-only protocol log. About 40 lines.

**5. Optional re-execution verifier.** If `verify_enabled` is set, re-executes the inference against the same registered model and compares the output hash. If mismatch detected, submits an `ObserverDisputeTx`. About 30 lines.

**6. Epoch attestor.** At epoch boundary, submits a signed `ProtocolAttestation { epoch, jobs_observed, jobs_verified }` to prove the observer was online and recording events. Required for observer reward eligibility. About 10 lines.

The total validator client is approximately 600–1000 lines of code (serving + observer modes), not counting the consensus engine.

---

## What backend should validators use

This is a business decision for each validator. The economics work as follows: when a validator is VRF-elected as the serving validator, they earn 50% of the job revenue. When they are in observer mode, they earn a share of the 30% observer pool. Their cost is whatever they pay for inference (only when serving — observer mode has negligible compute cost).

Validators with low traffic will prefer pay-per-token APIs like Together.ai or Fireworks.ai, where they only pay when a job arrives. Validators with high traffic will find it cheaper to rent dedicated GPU capacity from RunPod or Vast.ai on a monthly basis. Very high-volume validators may eventually buy hardware outright.

The revenue-sharing model means validators earn something every slot — serving revenue when elected, observer revenue otherwise. This smooths out income compared to a model where only the elected validator earns. The protocol enforces correctness: if your backend returns wrong outputs and you commit to them, any observer or user can dispute, the block gets reverted, and your bond is at risk.

---

## Revenue sharing between serving and observer validators

The serving validator shares job revenue with all observer validators who are actively protocolling. This aligns incentives: observers are motivated to stay online and watch because they earn a share, and the serving validator benefits from the network's attestation which increases user trust.

**Per-job revenue split:**

| Recipient | Share | Condition |
|---|---|---|
| Serving validator | 50% | Served the job correctly (not disputed/reverted) |
| Observer pool | 30% | Split equally among observers with valid `ProtocolAttestation` for the epoch |
| Delegator pool | 15% | Proportional to stake |
| Dispute bounty reserve | 5% | Accumulated; paid to successful disputers |

If a serving validator is slashed or has a block reverted during the epoch, their entire epoch rewards (serving + observer) are forfeited and redistributed to the observer pool.

Observers who successfully file a dispute that results in slashing receive a bounty from the slashed bond, in addition to their regular observer rewards.

---

## Changes to the consensus and slot structure

The slot time can now be reduced. Without ZK proof generation (which takes 8–12 seconds), a slot only needs to accommodate:

- Network round-trip to inference backend: 1–3 seconds
- BFT committee attestation: 3–4 seconds
- Block propagation: 1–2 seconds

A **6-second slot** is achievable. This is close to Ethereum's current 12-second slot, so the same engineering assumptions apply.

The slot structure has three phases:

**Phase 1 (0–1s): Commit.** Users submit job requests. Proposer seals the batch with a commitment root. Commit-reveal still applies — the proposer cannot read prompt content before the batch is sealed.

**Phase 2 (1–4s): Route and respond (single server).** The single VRF-elected serving validator routes each job to its backend and collects outputs. It submits an `OutputCommitment` (hash) on-chain. `PromptRevealed` and `ResponseCommitted` events are emitted for observer validators to record.

**Phase 3 (4–6s): BFT commit + observer protocolling.** 128-validator BFT committee checks that the commitment is well-formed and comes from the elected serving validator. Block finalised via CometBFT prevote/precommit. Observer validators record the prompt/response events in their local protocol logs.

**Post-slot: Dispute window (7 slots).** If a user or observer detects a problem, they can file a dispute within 7 slots. The serving validator has the dispute window to submit a justification. If no valid justification, a consensus vote is triggered (3-slot vote window). If 2/3+ uphold, the block is reverted and the serving validator is slashed.

---

## Attack surface specific to the single-server model

The single-server model introduces some attack vectors that differ from the redundant-execution model.

**Backend manipulation.** A serving validator's inference backend could be compromised (API key stolen, self-hosted machine hacked) and return subtly wrong outputs. The validator is still economically responsible because they signed the commitment. Unlike the previous 3-validator model, there is no automatic majority-agreement check within the slot — but observer validators who re-execute inference will detect the divergence and file a dispute. The consensus vote mechanism then reverts the block and slashes the serving validator.

**Single point of failure per slot.** Since only one validator serves each job, a compromised server can return wrong outputs to users within the slot. The mitigation is that users receive the output *and* can see the on-chain commitment hash — they can independently verify the hash matches and file a dispute immediately if it doesn't (triggering immediate reversion without a vote). For output quality issues (correct hash but bad content), observers with re-execution enabled provide a safety net.

**Observer collusion.** In theory, 2/3+ of validators could collude to uphold a false dispute and revert an honest serving validator's block. The mitigation is economic: the dispute filing requires a deposit, and the 2/3+ collusion threshold is the same BFT assumption that secures the entire chain. If 2/3+ of stake is malicious, the chain's security guarantees are broken regardless of the dispute mechanism.

**Latency-based censorship.** A serving validator could deliberately use a slow backend so their commitment always arrives too late to be included in the block. The mitigation is a per-slot commitment deadline (4 seconds): commitments arriving after the deadline are treated as missing, triggering the inactivity penalty. The job is re-queued for the next slot's serving validator. A validator who consistently misses deadlines is eventually ejected by the inactivity leak.

**API key exposure.** Validators using commercial APIs have API keys that, if leaked, allow attackers to burn their compute budget without their knowledge. This is a standard operational security concern, not a protocol-level attack. Validators are responsible for key rotation and rate limiting on their backend accounts.

**Dispute spam / griefing.** A malicious user or observer could file frivolous disputes to force consensus votes and waste validator attention. The mitigation is a dispute deposit (burned if dismissed) and the requirement that observer disputes include a verifiable re-execution output hash — observers cannot dispute without evidence.

---

## Updated parameter table

| Parameter | Value | Notes |
|---|---|---|
| Slot time | 6 seconds | No ZK generation needed |
| Epoch length | 14,400 slots (~24h) | Daily reward distribution |
| Min stake | 5,000 BIGT | Lower barrier, no GPU hardware cost |
| Serving validators per job | **1** (VRF elected) | Single server, not redundant |
| Observer validators per job | **All other active** | Event-driven protocolling |
| Dispute window | 7 slots (~42s) | Time for serving validator to justify |
| Dispute vote window | 3 slots (~18s) | Time for validators to cast votes |
| Dispute vote threshold | 2/3+ of active stake | Weighted by stake |
| Slash: wrong commitment (user) | 100% bond + block reversion | Immediate, cryptographically provable |
| Slash: observer mismatch | 10% bond + block reversion | Requires 2/3+ consensus vote |
| Slash: equivocation | 100% bond + block reversion | Immediate, self-evident |
| Commitment deadline | 4 seconds into slot | Late = treated as missing |
| Inactivity threshold | 4 consecutive epochs | Before quadratic drain starts |
| Max validator share | 5% of total stake | Unchanged |
| Unbonding period | 21 days | Unchanged |
| Serving validator reward | 50% of job revenue | Revenue sharing |
| Observer pool reward | 30% of job revenue | Split among attestors |
| Delegator pool reward | 15% of job revenue | Proportional to stake |
| Dispute bounty reserve | 5% of job revenue | Paid on successful disputes |

---

## Summary of what is simpler and what remains the same

What became simpler: no GPU required, no ZK proof generation, no complex circuit compilation, no Plonky2/Halo2 toolchain, only 1 validator runs inference per job (down from 3 — saving 2/3 of compute costs), validator client is a dual-mode binary (~1000 lines), slot time halved to 6 seconds.

What is new: observer event protocolling (all validators record prompts/responses via on-chain events), consensus-backed dispute resolution (2/3+ validator vote to revert blocks), block reversion mechanics (state rollback + job re-queue), revenue sharing (serving validator shares 50/30/15/5 with observers/delegators/bounty reserve).

What stayed the same: BFT consensus (CometBFT), VRF slot election, commit-reveal for MEV protection, slashing for equivocation, inactivity leak, KES key evolution, weak subjectivity checkpoints anchored to L1, model registry with governance approval, 5% validator cap.

The protocol becomes significantly more efficient (1 inference per job instead of 3) while retaining economic security through observer protocolling and consensus-backed reversion. The trust assumption is: "The serving validator is honest by default; if they're not, any user or observer can challenge them and 2/3+ of the network will revert the damage."