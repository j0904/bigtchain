// Package subscription manages user accounts, balances, and monthly subscriptions.
// Users deposit uBIGT, subscribe to a plan (basic/pro/enterprise), and consume
// jobs against their monthly quota. The module enforces payment and limits.
package subscription

import (
	"encoding/json"
	"fmt"

	"github.com/bigtchain/bigt/chain/store"
	"github.com/bigtchain/bigt/chain/types"
)

var (
	accountPrefix = []byte("acct/")
	subPrefix     = []byte("sub/")
)

func accountKey(addr string) []byte { return append(accountPrefix, []byte(addr)...) }
func subKey(addr string) []byte     { return append(subPrefix, []byte(addr)...) }

// Module manages accounts and subscriptions.
type Module struct {
	store *store.Store
}

// New creates a new subscription module.
func New(s *store.Store) *Module {
	return &Module{store: s}
}

// Deposit credits uBIGT to a user account (creates account if needed).
func (m *Module) Deposit(tx types.DepositTx) error {
	if tx.Amount <= 0 {
		return fmt.Errorf("deposit amount must be positive")
	}
	acct, err := m.GetAccount(tx.UserAddr)
	if err != nil {
		return err
	}
	if acct == nil {
		acct = &types.Account{Address: tx.UserAddr}
	}
	acct.Balance += tx.Amount
	return m.putAccount(acct)
}

// Subscribe creates or renews a monthly subscription plan.
// Deducts the plan fee from the user's balance.
func (m *Module) Subscribe(tx types.SubscribeTx, currentSlot int64) error {
	acct, err := m.GetAccount(tx.UserAddr)
	if err != nil {
		return err
	}
	if acct == nil {
		return fmt.Errorf("account %s not found; deposit first", tx.UserAddr)
	}

	fee, limit := planCost(tx.Plan)
	if fee == 0 {
		return fmt.Errorf("unknown plan: %s", tx.Plan)
	}
	if acct.Balance < fee {
		return fmt.Errorf("insufficient balance: have %d uBIGT, need %d", acct.Balance, fee)
	}

	// Check for existing active subscription — extend or replace.
	existing, err := m.GetSubscription(tx.UserAddr)
	if err != nil {
		return err
	}
	startSlot := currentSlot
	if existing != nil && existing.ExpiresSlot > currentSlot {
		// Already has an active sub; renewal starts when current expires.
		startSlot = existing.ExpiresSlot
	}

	acct.Balance -= fee
	if err := m.putAccount(acct); err != nil {
		return err
	}

	sub := &types.Subscription{
		UserAddr:    tx.UserAddr,
		Plan:        tx.Plan,
		StartSlot:   startSlot,
		ExpiresSlot: startSlot + types.SubscriptionDurationSlots,
		JobsUsed:    0,
		JobsLimit:   limit,
		PaidAmount:  fee,
		AutoRenew:   tx.AutoRenew,
	}
	return m.putSubscription(sub)
}

// CancelAutoRenew disables auto-renewal; sub remains active until expiry.
func (m *Module) CancelAutoRenew(addr string) error {
	sub, err := m.GetSubscription(addr)
	if err != nil {
		return err
	}
	if sub == nil {
		return fmt.Errorf("no subscription found for %s", addr)
	}
	sub.AutoRenew = false
	return m.putSubscription(sub)
}

// ConsumeJob checks that the user has an active subscription with remaining quota,
// and increments the usage counter. Returns error if not allowed.
func (m *Module) ConsumeJob(userAddr string, currentSlot int64) error {
	sub, err := m.GetSubscription(userAddr)
	if err != nil {
		return err
	}
	if sub == nil {
		return fmt.Errorf("no active subscription for %s", userAddr)
	}
	if currentSlot < sub.StartSlot || currentSlot >= sub.ExpiresSlot {
		return fmt.Errorf("subscription for %s expired at slot %d (current: %d)", userAddr, sub.ExpiresSlot, currentSlot)
	}
	if sub.JobsLimit > 0 && sub.JobsUsed >= sub.JobsLimit {
		return fmt.Errorf("monthly job quota exhausted for %s (%d/%d)", userAddr, sub.JobsUsed, sub.JobsLimit)
	}
	sub.JobsUsed++
	return m.putSubscription(sub)
}

// ProcessAutoRenewals renews expired subscriptions with auto_renew=true.
// Called at epoch boundaries. Returns count of renewed subscriptions.
func (m *Module) ProcessAutoRenewals(currentSlot int64) (int, error) {
	var renewed int
	var toRenew []string

	err := m.store.Scan(subPrefix, func(_, val []byte) bool {
		var sub types.Subscription
		if err := json.Unmarshal(val, &sub); err != nil {
			return true
		}
		if sub.AutoRenew && currentSlot >= sub.ExpiresSlot {
			toRenew = append(toRenew, sub.UserAddr)
		}
		return true
	})
	if err != nil {
		return 0, err
	}

	for _, addr := range toRenew {
		sub, err := m.GetSubscription(addr)
		if err != nil || sub == nil {
			continue
		}
		err = m.Subscribe(types.SubscribeTx{
			UserAddr:  addr,
			Plan:      sub.Plan,
			AutoRenew: true,
		}, currentSlot)
		if err != nil {
			// Insufficient balance for renewal — disable auto-renew.
			sub.AutoRenew = false
			_ = m.putSubscription(sub)
			continue
		}
		renewed++
	}
	return renewed, nil
}

// GetAccount retrieves a user account. Returns (nil, nil) if not found.
func (m *Module) GetAccount(addr string) (*types.Account, error) {
	data, err := m.store.Get(accountKey(addr))
	if err != nil || data == nil {
		return nil, err
	}
	var a types.Account
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetSubscription retrieves a user's subscription. Returns (nil, nil) if not found.
func (m *Module) GetSubscription(addr string) (*types.Subscription, error) {
	data, err := m.store.Get(subKey(addr))
	if err != nil || data == nil {
		return nil, err
	}
	var s types.Subscription
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Module) putAccount(a *types.Account) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return m.store.Set(accountKey(a.Address), data)
}

func (m *Module) putSubscription(s *types.Subscription) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return m.store.Set(subKey(s.UserAddr), data)
}

func planCost(plan types.SubscriptionPlan) (fee, limit int64) {
	switch plan {
	case types.PlanBasic:
		return types.SubscriptionBasicMonthly, types.SubscriptionBasicJobs
	case types.PlanPro:
		return types.SubscriptionProMonthly, types.SubscriptionProJobs
	case types.PlanEnterprise:
		return types.SubscriptionEnterpriseMonthly, types.SubscriptionEnterpriseJobs
	default:
		return 0, 0
	}
}
