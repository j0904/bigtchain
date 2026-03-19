// Package app wires together all chain modules into a CometBFT ABCI application.
package app

import (
"context"
"encoding/json"
"fmt"
"os"

abcitypes "github.com/cometbft/cometbft/abci/types"

"github.com/bigtchain/bigt/chain/genesis"
"github.com/bigtchain/bigt/chain/modules/jobs"
"github.com/bigtchain/bigt/chain/modules/rewards"
"github.com/bigtchain/bigt/chain/modules/slashing"
"github.com/bigtchain/bigt/chain/modules/staking"
"github.com/bigtchain/bigt/chain/modules/subscription"
"github.com/bigtchain/bigt/chain/modules/vrf"
"github.com/bigtchain/bigt/chain/registry"
"github.com/bigtchain/bigt/chain/store"
"github.com/bigtchain/bigt/chain/types"
)

// App is the BIGT ABCI application.
type App struct {
abcitypes.BaseApplication

store    *store.Store
staking  *staking.Module
jobs     *jobs.Module
registry *registry.Registry
rewards  *rewards.Module
slashing *slashing.Handler
subscription *subscription.Module

currentSlot  int64
currentEpoch int64
epochSeed    []byte
proposer     string
server       string // VRF-elected serving validator (single-server architecture)
slashEvents  []slashing.Event

chainID     string
totalSupply int64
activeVals  []*staking.Validator
}

func New(dataDir string) (*App, error) {
s, err := store.Open(dataDir)
if err != nil {
return nil, fmt.Errorf("open store: %w", err)
}
st := staking.New(s)
jm := jobs.New(s)
reg := registry.New(s, 3)
rm := rewards.New(s, st)
sh := slashing.New(st, jm)
sub := subscription.New(s)
return &App{store: s, staking: st, jobs: jm, registry: reg, rewards: rm, slashing: sh, subscription: sub}, nil
}

func (a *App) Info(_ context.Context, _ *abcitypes.RequestInfo) (*abcitypes.ResponseInfo, error) {
return &abcitypes.ResponseInfo{Data: fmt.Sprintf(`{"chain_id":%q}`, a.chainID), Version: "0.1.0"}, nil
}

func (a *App) InitChain(_ context.Context, req *abcitypes.RequestInitChain) (*abcitypes.ResponseInitChain, error) {
var g genesis.Genesis
if err := json.Unmarshal(req.AppStateBytes, &g); err != nil {
return nil, fmt.Errorf("bad app state: %w", err)
}
a.chainID = g.ChainID
a.totalSupply = g.TotalSupply
for _, vg := range g.Validators {
if err := a.staking.RegisterValidator(staking.MsgRegisterValidator{
Address: vg.Address, ConsensusPubKey: vg.PubKey, BLSPubKey: vg.BLSPubKey,
Bond: vg.Bond, Commission: vg.Commission, Moniker: vg.Moniker,
}, g.TotalSupply); err != nil {
return nil, fmt.Errorf("register validator %s: %w", vg.Address, err)
}
}
return &abcitypes.ResponseInitChain{}, nil
}

func (a *App) FinalizeBlock(_ context.Context, req *abcitypes.RequestFinalizeBlock) (*abcitypes.ResponseFinalizeBlock, error) {
a.currentSlot = req.Height
a.currentEpoch = a.currentSlot / types.EpochSlots
a.slashEvents = nil

var err error
a.activeVals, err = a.staking.ListActive()
if err != nil {
fmt.Fprintf(os.Stderr, "FinalizeBlock: list validators: %v\n", err)
}

a.epochSeed = vrf.EpochSeed(req.ProposerAddress, a.currentEpoch)
a.proposer, a.server = vrf.ElectSlot(a.epochSeed, a.currentSlot, a.activeVals)

if _, err := a.staking.ProcessMatureUnbondings(a.currentSlot); err != nil {
fmt.Fprintf(os.Stderr, "FinalizeBlock: process unbondings: %v\n", err)
}
if a.currentSlot > 0 && a.currentSlot%types.EpochSlots == 0 {
a.endEpoch(a.currentEpoch - 1)
}

// Tally expired dispute votes before processing new transactions.
slashEvts, err := a.slashing.TallyExpiredDisputes(a.currentSlot)
if err != nil {
fmt.Fprintf(os.Stderr, "FinalizeBlock: tally disputes: %v\n", err)
}
for _, ev := range slashEvts {
a.slashEvents = append(a.slashEvents, ev)
if err := a.rewards.RecordJobSlashed(ev.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "record slashed from tally: %v\n", err)
}
if err := a.rewards.RecordBlockReverted(ev.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "record reverted from tally: %v\n", err)
}
}

txResults := make([]*abcitypes.ExecTxResult, len(req.Txs))
for i, rawTx := range req.Txs {
txResults[i] = a.deliverTx(rawTx)
}

_, missed, err := a.jobs.FinaliseSlot(a.currentSlot)
if err != nil {
fmt.Fprintf(os.Stderr, "FinalizeBlock: finalise slot: %v\n", err)
}
for addr := range missed {
if err := a.staking.IncrementMissed(addr); err != nil {
fmt.Fprintf(os.Stderr, "increment missed for %s: %v\n", addr, err)
}
}

return &abcitypes.ResponseFinalizeBlock{TxResults: txResults}, nil
}

func (a *App) endEpoch(epoch int64) {
epochRewards, err := a.rewards.DistributeEpoch(epoch, a.totalSupply, 0)
if err != nil {
fmt.Fprintf(os.Stderr, "endEpoch %d: distribute rewards: %v\n", epoch, err)
}
_ = epochRewards

for _, v := range a.activeVals {
epochsSinceActive := epoch - v.LastActiveEpoch
if epochsSinceActive >= types.InactivityThresholdEpochs && v.Bond > 0 {
excess := epochsSinceActive - types.InactivityThresholdEpochs
drainBPS := excess * excess * 100 / 1000
if drainBPS > 10_000 {
drainBPS = 10_000
}
if _, err := a.staking.SlashBond(v.Address, drainBPS); err != nil {
fmt.Fprintf(os.Stderr, "inactivity drain for %s: %v\n", v.Address, err)
}
}
}
}

func (a *App) deliverTx(rawTx []byte) *abcitypes.ExecTxResult {
var tx types.Tx
if err := json.Unmarshal(rawTx, &tx); err != nil {
return errResult(1, "invalid tx: "+err.Error())
}
switch tx.Type {
case types.TxJobRequest:
return a.handleJobRequest(tx)
case types.TxReveal:
return a.handleReveal(tx)
case types.TxCommitment:
return a.handleCommitment(tx)
case types.TxDispute:
return a.handleDispute(tx)
case types.TxObserverDispute:
return a.handleObserverDispute(tx)
case types.TxDisputeVote:
return a.handleDisputeVote(tx)
case types.TxProtocolAttestation:
return a.handleProtocolAttestation(tx)
case types.TxRegValidator:
return a.handleRegisterValidator(tx)
case types.TxDelegate:
return a.handleDelegate(tx)
case types.TxUndelegate:
return a.handleUndelegate(tx)
case types.TxUnjail:
return a.handleUnjail(tx)
case types.TxProposeModel:
return a.handleProposeModel(tx)
case types.TxApproveModel:
return a.handleApproveModel(tx)
case types.TxDeposit:
return a.handleDeposit(tx)
case types.TxSubscribe:
return a.handleSubscribe(tx)
case types.TxCancelSubscription:
return a.handleCancelSubscription(tx)
default:
return errResult(2, "unknown tx type: "+string(tx.Type))
}
}

func (a *App) handleJobRequest(tx types.Tx) *abcitypes.ExecTxResult {
var req types.JobRequest
if err := json.Unmarshal(tx.Payload, &req); err != nil {
return errResult(10, err.Error())
}
valid, err := a.registry.IsValid(req.ModelID, a.currentSlot)
if err != nil {
return errResult(11, err.Error())
}
if !valid {
return errResult(12, "model not registered: "+req.ModelID)
}
// Enforce active subscription with remaining quota.
if err := a.subscription.ConsumeJob(req.UserAddr, a.currentSlot); err != nil {
return errResult(14, "subscription check failed: "+err.Error())
}
if err := a.jobs.Commit(req, a.currentSlot, a.server); err != nil {
return errResult(13, err.Error())
}
return okResult("job committed: " + req.JobID)
}

func (a *App) handleReveal(tx types.Tx) *abcitypes.ExecTxResult {
var rev types.RevealTx
if err := json.Unmarshal(tx.Payload, &rev); err != nil {
return errResult(20, err.Error())
}
if err := a.jobs.Reveal(rev); err != nil {
return errResult(21, err.Error())
}
return okResult("revealed: " + rev.JobID)
}

func (a *App) handleCommitment(tx types.Tx) *abcitypes.ExecTxResult {
var c types.OutputCommitment
if err := json.Unmarshal(tx.Payload, &c); err != nil {
return errResult(30, err.Error())
}
if err := a.jobs.AddCommitment(c); err != nil {
return errResult(31, err.Error())
}
if err := a.staking.ResetMissed(c.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "reset missed for %s: %v\n", c.ValidatorAddr, err)
}
if err := a.rewards.RecordJobServed(c.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "record job served: %v\n", err)
}
return okResult(fmt.Sprintf("job %s served by %s", c.JobID, c.ValidatorAddr))
}

func (a *App) handleDispute(tx types.Tx) *abcitypes.ExecTxResult {
var d types.DisputeTx
if err := json.Unmarshal(tx.Payload, &d); err != nil {
return errResult(40, err.Error())
}
ev, err := a.slashing.ProcessUserDispute(d, a.currentSlot)
if err != nil {
return errResult(41, err.Error())
}
if ev == nil {
return errResult(42, "dispute invalid: hashes match")
}
a.slashEvents = append(a.slashEvents, *ev)
if err := a.rewards.RecordJobSlashed(d.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "record slashed: %v\n", err)
}
if err := a.rewards.RecordBlockReverted(d.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "record reverted: %v\n", err)
}
return okResult(fmt.Sprintf("slashed %d uBIGT from %s", ev.AmountSlashed, ev.ValidatorAddr))
}

func (a *App) handleObserverDispute(tx types.Tx) *abcitypes.ExecTxResult {
var d types.ObserverDisputeTx
if err := json.Unmarshal(tx.Payload, &d); err != nil {
return errResult(43, err.Error())
}
if err := a.slashing.ProcessObserverDispute(d, a.currentSlot); err != nil {
return errResult(44, err.Error())
}
return okResult(fmt.Sprintf("observer dispute opened for job %s by %s", d.JobID, d.ObserverAddr))
}

func (a *App) handleDisputeVote(tx types.Tx) *abcitypes.ExecTxResult {
var v types.DisputeVoteTx
if err := json.Unmarshal(tx.Payload, &v); err != nil {
return errResult(45, err.Error())
}
if err := a.slashing.CastVote(v); err != nil {
return errResult(46, err.Error())
}
return okResult(fmt.Sprintf("vote recorded: %s on job %s by %s", v.Vote, v.JobID, v.VoterAddr))
}

func (a *App) handleProtocolAttestation(tx types.Tx) *abcitypes.ExecTxResult {
var att types.ProtocolAttestation
if err := json.Unmarshal(tx.Payload, &att); err != nil {
return errResult(47, err.Error())
}
// Record observer attestation for reward calculation.
if err := a.rewards.RecordJobObserved(att.ObserverAddr, att.Epoch); err != nil {
fmt.Fprintf(os.Stderr, "record observer attestation: %v\n", err)
}
return okResult(fmt.Sprintf("attestation from %s: %d jobs observed", att.ObserverAddr, att.JobsObserved))
}

func (a *App) handleRegisterValidator(tx types.Tx) *abcitypes.ExecTxResult {
var msg staking.MsgRegisterValidator
if err := json.Unmarshal(tx.Payload, &msg); err != nil {
return errResult(50, err.Error())
}
if err := a.staking.RegisterValidator(msg, a.totalSupply); err != nil {
return errResult(51, err.Error())
}
return okResult("validator registered: " + msg.Address)
}

func (a *App) handleDelegate(tx types.Tx) *abcitypes.ExecTxResult {
var msg staking.MsgDelegate
if err := json.Unmarshal(tx.Payload, &msg); err != nil {
return errResult(60, err.Error())
}
if err := a.staking.Delegate(msg, a.currentSlot); err != nil {
return errResult(61, err.Error())
}
return okResult("delegated")
}

func (a *App) handleUndelegate(tx types.Tx) *abcitypes.ExecTxResult {
var msg staking.MsgUndelegate
if err := json.Unmarshal(tx.Payload, &msg); err != nil {
return errResult(70, err.Error())
}
if err := a.staking.Undelegate(msg, a.currentSlot); err != nil {
return errResult(71, err.Error())
}
return okResult("unbonding queued")
}

func (a *App) handleUnjail(tx types.Tx) *abcitypes.ExecTxResult {
var msg staking.MsgUnjail
if err := json.Unmarshal(tx.Payload, &msg); err != nil {
return errResult(80, err.Error())
}
if err := a.staking.Unjail(msg.ValidatorAddr); err != nil {
return errResult(81, err.Error())
}
return okResult("unjailed: " + msg.ValidatorAddr)
}

func (a *App) handleProposeModel(tx types.Tx) *abcitypes.ExecTxResult {
var msg registry.MsgProposeModel
if err := json.Unmarshal(tx.Payload, &msg); err != nil {
return errResult(90, err.Error())
}
if err := a.registry.ProposeModel(msg); err != nil {
return errResult(91, err.Error())
}
return okResult("model proposed: " + msg.ModelID)
}

func (a *App) handleApproveModel(tx types.Tx) *abcitypes.ExecTxResult {
var msg registry.MsgApproveModel
if err := json.Unmarshal(tx.Payload, &msg); err != nil {
return errResult(100, err.Error())
}
if err := a.registry.ApproveModel(msg, a.currentSlot); err != nil {
return errResult(101, err.Error())
}
return okResult("model vote recorded: " + msg.ModelID)
}

func (a *App) handleDeposit(tx types.Tx) *abcitypes.ExecTxResult {
var d types.DepositTx
if err := json.Unmarshal(tx.Payload, &d); err != nil {
return errResult(110, err.Error())
}
if err := a.subscription.Deposit(d); err != nil {
return errResult(111, err.Error())
}
return okResult(fmt.Sprintf("deposited %d uBIGT to %s", d.Amount, d.UserAddr))
}

func (a *App) handleSubscribe(tx types.Tx) *abcitypes.ExecTxResult {
var s types.SubscribeTx
if err := json.Unmarshal(tx.Payload, &s); err != nil {
return errResult(120, err.Error())
}
if err := a.subscription.Subscribe(s, a.currentSlot); err != nil {
return errResult(121, err.Error())
}
return okResult(fmt.Sprintf("subscribed %s to plan %s", s.UserAddr, s.Plan))
}

func (a *App) handleCancelSubscription(tx types.Tx) *abcitypes.ExecTxResult {
var c types.CancelSubscriptionTx
if err := json.Unmarshal(tx.Payload, &c); err != nil {
return errResult(130, err.Error())
}
if err := a.subscription.CancelAutoRenew(c.UserAddr); err != nil {
return errResult(131, err.Error())
}
return okResult("subscription auto-renew cancelled for " + c.UserAddr)
}

func (a *App) Query(_ context.Context, req *abcitypes.RequestQuery) (*abcitypes.ResponseQuery, error) {
switch req.Path {
case "/validators":
vals, err := a.staking.ListAll()
if err != nil {
return &abcitypes.ResponseQuery{Code: 1, Log: err.Error()}, nil
}
data, _ := json.Marshal(vals)
return &abcitypes.ResponseQuery{Value: data}, nil
case "/job":
j, err := a.jobs.GetJob(string(req.Data))
if err != nil || j == nil {
return &abcitypes.ResponseQuery{Code: 1, Log: "job not found"}, nil
}
data, _ := json.Marshal(j)
return &abcitypes.ResponseQuery{Value: data}, nil
case "/account":
acct, err := a.subscription.GetAccount(string(req.Data))
if err != nil || acct == nil {
return &abcitypes.ResponseQuery{Code: 1, Log: "account not found"}, nil
}
data, _ := json.Marshal(acct)
return &abcitypes.ResponseQuery{Value: data}, nil
case "/subscription":
sub, err := a.subscription.GetSubscription(string(req.Data))
if err != nil || sub == nil {
return &abcitypes.ResponseQuery{Code: 1, Log: "subscription not found"}, nil
}
data, _ := json.Marshal(sub)
return &abcitypes.ResponseQuery{Value: data}, nil
default:
return &abcitypes.ResponseQuery{Code: 1, Log: "unknown path: " + req.Path}, nil
}
}

func (a *App) CheckTx(_ context.Context, _ *abcitypes.RequestCheckTx) (*abcitypes.ResponseCheckTx, error) {
return &abcitypes.ResponseCheckTx{Code: 0}, nil
}

func (a *App) Commit(_ context.Context, _ *abcitypes.RequestCommit) (*abcitypes.ResponseCommit, error) {
return &abcitypes.ResponseCommit{}, nil
}

func okResult(log string) *abcitypes.ExecTxResult  { return &abcitypes.ExecTxResult{Code: 0, Log: log} }
func errResult(code uint32, log string) *abcitypes.ExecTxResult {
return &abcitypes.ExecTxResult{Code: code, Log: log}
}
