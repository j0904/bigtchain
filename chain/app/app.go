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

currentSlot  int64
currentEpoch int64
epochSeed    []byte
proposer     string
workers      []string
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
return &App{store: s, staking: st, jobs: jm, registry: reg, rewards: rm, slashing: sh}, nil
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
a.proposer, a.workers = vrf.ElectSlot(a.epochSeed, a.currentSlot, a.activeVals)

if _, err := a.staking.ProcessMatureUnbondings(a.currentSlot); err != nil {
fmt.Fprintf(os.Stderr, "FinalizeBlock: process unbondings: %v\n", err)
}
if a.currentSlot > 0 && a.currentSlot%types.EpochSlots == 0 {
a.endEpoch(a.currentEpoch - 1)
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
if err := a.jobs.Commit(req, a.currentSlot, a.workers); err != nil {
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
agreed, err := a.jobs.AddCommitment(c)
if err != nil {
return errResult(31, err.Error())
}
if err := a.staking.ResetMissed(c.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "reset missed for %s: %v\n", c.ValidatorAddr, err)
}
if agreed {
if err := a.rewards.RecordJobServed(c.ValidatorAddr, a.currentEpoch); err != nil {
fmt.Fprintf(os.Stderr, "record job served: %v\n", err)
}
return okResult(fmt.Sprintf("job %s agreed", c.JobID))
}
return okResult("commitment accepted; awaiting majority")
}

func (a *App) handleDispute(tx types.Tx) *abcitypes.ExecTxResult {
var d types.DisputeTx
if err := json.Unmarshal(tx.Payload, &d); err != nil {
return errResult(40, err.Error())
}
ev, err := a.slashing.ProcessUserDispute(d)
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
return okResult(fmt.Sprintf("slashed %d uBIGT from %s", ev.AmountSlashed, ev.ValidatorAddr))
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
