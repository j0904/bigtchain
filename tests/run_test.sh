#!/usr/bin/env bash
# run_test.sh — Full Docker integration test for 3-validator BIGT network
# with 3 users subscribing to monthly plans (basic/pro/enterprise).
#
# Usage: ./tests/run_test.sh
# Env:   BIGT_RPC (default: http://localhost:26657)
#        SKIP_BUILD=1  — skip docker compose build
#        SKIP_CLEANUP=1 — leave containers running after test
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"

export BIGT_RPC="${BIGT_RPC:-http://localhost:26657}"

# shellcheck source=helpers.sh
source "${SCRIPT_DIR}/helpers.sh"

PASS=0
FAIL=0
TESTS=0

assert() {
  local desc="$1"
  shift
  TESTS=$((TESTS + 1))
  if "$@"; then
    PASS=$((PASS + 1))
    echo "  ✓ $desc"
  else
    FAIL=$((FAIL + 1))
    echo "  ✗ $desc"
  fi
}

assert_tx() {
  local result="$1"
  local desc="$2"
  TESTS=$((TESTS + 1))
  if check_tx_success "$result" "$desc" >/dev/null 2>&1; then
    PASS=$((PASS + 1))
    echo "  ✓ $desc"
  else
    FAIL=$((FAIL + 1))
    echo "  ✗ $desc"
  fi
}

assert_eq() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  TESTS=$((TESTS + 1))
  if [ "$expected" = "$actual" ]; then
    PASS=$((PASS + 1))
    echo "  ✓ $desc (=$expected)"
  else
    FAIL=$((FAIL + 1))
    echo "  ✗ $desc (expected=$expected, got=$actual)"
  fi
}

assert_ge() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  TESTS=$((TESTS + 1))
  if [ "$actual" -ge "$expected" ] 2>/dev/null; then
    PASS=$((PASS + 1))
    echo "  ✓ $desc ($actual >= $expected)"
  else
    FAIL=$((FAIL + 1))
    echo "  ✗ $desc (expected >= $expected, got=$actual)"
  fi
}

# ───────────────────────────────────────────────────────────────────
# Lifecycle
# ───────────────────────────────────────────────────────────────────

cleanup() {
  if [ "${SKIP_CLEANUP:-}" = "1" ]; then
    echo ""
    echo "SKIP_CLEANUP=1 — containers left running."
    return
  fi
  echo ""
  echo "Tearing down Docker containers..."
  docker compose -f "$COMPOSE_FILE" down -v --remove-orphans 2>/dev/null || true
}

trap cleanup EXIT

echo "============================================"
echo " BIGT Docker Integration Test"
echo " 3 Validators · 3 Users · Monthly Subscriptions"
echo "============================================"
echo ""

# ───────────────────────────────────────────────────────────────────
# 1. Build & start the 3-validator network
# ───────────────────────────────────────────────────────────────────

echo "── Phase 1: Start 3-validator network ──"

if [ "${SKIP_BUILD:-}" != "1" ]; then
  echo "Building Docker images..."
  docker compose -f "$COMPOSE_FILE" build --quiet
fi

# Clean any previous testnet state
docker compose -f "$COMPOSE_FILE" down -v --remove-orphans 2>/dev/null || true

echo "Starting validators..."
docker compose -f "$COMPOSE_FILE" up -d

# Wait for chain to produce blocks
wait_for_chain 90
wait_blocks 2

echo ""

# ───────────────────────────────────────────────────────────────────
# 2. Verify validators are running
# ───────────────────────────────────────────────────────────────────

echo "── Phase 2: Verify validator network ──"

STATUS=$(curl -sf "${BIGT_RPC}/status")
CHAIN_ID=$(echo "$STATUS" | jq -r '.result.node_info.network')
HEIGHT=$(echo "$STATUS" | jq -r '.result.sync_info.latest_block_height')

assert_eq "Chain ID" "bigt-docker-test" "$CHAIN_ID"
assert_ge "Block height > 0" "1" "$HEIGHT"

# Check all 3 validator RPC endpoints respond
for port in 26657 26658 26659; do
  NODE_STATUS=$(curl -sf "http://localhost:${port}/status" 2>/dev/null || echo "")
  if [ -n "$NODE_STATUS" ]; then
    NODE_HEIGHT=$(echo "$NODE_STATUS" | jq -r '.result.sync_info.latest_block_height')
    assert_ge "Validator on port ${port} synced" "1" "$NODE_HEIGHT"
  else
    TESTS=$((TESTS + 1))
    FAIL=$((FAIL + 1))
    echo "  ✗ Validator on port ${port} not reachable"
  fi
done

echo ""

# ───────────────────────────────────────────────────────────────────
# 3. Define 3 test users
# ───────────────────────────────────────────────────────────────────

# User addresses (deterministic test addresses)
USER1="user1_test_basic_monthly"
USER2="user2_test_pro_monthly"
USER3="user3_test_enterprise_monthly"

# Deposit amounts (in uBIGT: 1 BIGT = 1,000,000 uBIGT)
# Basic=10 BIGT/mo, Pro=50 BIGT/mo, Enterprise=200 BIGT/mo
# Give each user enough for 3 months + extra
DEPOSIT_USER1=50000000      # 50 BIGT (enough for ~5 months basic)
DEPOSIT_USER2=200000000     # 200 BIGT (enough for ~4 months pro)
DEPOSIT_USER3=700000000     # 700 BIGT (enough for ~3.5 months enterprise)

echo "── Phase 3: Deposit funds for 3 users ──"

# Deposit for user 1
RESULT=$(send_deposit "$USER1" "$DEPOSIT_USER1")
assert_tx "$RESULT" "Deposit ${DEPOSIT_USER1} uBIGT to ${USER1}"

# Deposit for user 2
RESULT=$(send_deposit "$USER2" "$DEPOSIT_USER2")
assert_tx "$RESULT" "Deposit ${DEPOSIT_USER2} uBIGT to ${USER2}"

# Deposit for user 3
RESULT=$(send_deposit "$USER3" "$DEPOSIT_USER3")
assert_tx "$RESULT" "Deposit ${DEPOSIT_USER3} uBIGT to ${USER3}"

wait_blocks 1

# Verify balances via ABCI query
echo ""
echo "  Verifying account balances..."

ACCT1=$(query_account "$USER1")
BAL1=$(echo "$ACCT1" | jq -r '.balance')
assert_eq "User1 balance after deposit" "$DEPOSIT_USER1" "$BAL1"

ACCT2=$(query_account "$USER2")
BAL2=$(echo "$ACCT2" | jq -r '.balance')
assert_eq "User2 balance after deposit" "$DEPOSIT_USER2" "$BAL2"

ACCT3=$(query_account "$USER3")
BAL3=$(echo "$ACCT3" | jq -r '.balance')
assert_eq "User3 balance after deposit" "$DEPOSIT_USER3" "$BAL3"

echo ""

# ───────────────────────────────────────────────────────────────────
# 4. Subscribe each user to a different plan
# ───────────────────────────────────────────────────────────────────

echo "── Phase 4: Subscribe users to monthly plans ──"

# User 1 → Basic plan (10 BIGT/mo, 100 jobs, auto-renew ON)
RESULT=$(send_subscribe "$USER1" "basic" true)
assert_tx "$RESULT" "User1 subscribes to Basic plan"

# User 2 → Pro plan (50 BIGT/mo, 1000 jobs, auto-renew ON)
RESULT=$(send_subscribe "$USER2" "pro" true)
assert_tx "$RESULT" "User2 subscribes to Pro plan"

# User 3 → Enterprise plan (200 BIGT/mo, unlimited jobs, auto-renew OFF)
RESULT=$(send_subscribe "$USER3" "enterprise" false)
assert_tx "$RESULT" "User3 subscribes to Enterprise plan"

wait_blocks 1

# Verify subscriptions
echo ""
echo "  Verifying subscriptions..."

SUB1=$(query_subscription "$USER1")
SUB1_PLAN=$(echo "$SUB1" | jq -r '.plan')
SUB1_LIMIT=$(echo "$SUB1" | jq -r '.jobs_limit')
SUB1_AUTO=$(echo "$SUB1" | jq -r '.auto_renew')
assert_eq "User1 plan" "basic" "$SUB1_PLAN"
assert_eq "User1 job limit" "100" "$SUB1_LIMIT"
assert_eq "User1 auto-renew" "true" "$SUB1_AUTO"

SUB2=$(query_subscription "$USER2")
SUB2_PLAN=$(echo "$SUB2" | jq -r '.plan')
SUB2_LIMIT=$(echo "$SUB2" | jq -r '.jobs_limit')
SUB2_AUTO=$(echo "$SUB2" | jq -r '.auto_renew')
assert_eq "User2 plan" "pro" "$SUB2_PLAN"
assert_eq "User2 job limit" "1000" "$SUB2_LIMIT"
assert_eq "User2 auto-renew" "true" "$SUB2_AUTO"

SUB3=$(query_subscription "$USER3")
SUB3_PLAN=$(echo "$SUB3" | jq -r '.plan')
SUB3_LIMIT=$(echo "$SUB3" | jq -r '.jobs_limit')
SUB3_AUTO=$(echo "$SUB3" | jq -r '.auto_renew')
assert_eq "User3 plan" "enterprise" "$SUB3_PLAN"
assert_eq "User3 job limit" "0" "$SUB3_LIMIT"    # 0 = unlimited
assert_eq "User3 auto-renew" "false" "$SUB3_AUTO"

# Verify balances were deducted
ACCT1=$(query_account "$USER1")
BAL1_AFTER=$(echo "$ACCT1" | jq -r '.balance')
EXPECTED_BAL1=$((DEPOSIT_USER1 - 10000000))   # 50M - 10M = 40M
assert_eq "User1 balance after sub" "$EXPECTED_BAL1" "$BAL1_AFTER"

ACCT2=$(query_account "$USER2")
BAL2_AFTER=$(echo "$ACCT2" | jq -r '.balance')
EXPECTED_BAL2=$((DEPOSIT_USER2 - 50000000))   # 200M - 50M = 150M
assert_eq "User2 balance after sub" "$EXPECTED_BAL2" "$BAL2_AFTER"

ACCT3=$(query_account "$USER3")
BAL3_AFTER=$(echo "$ACCT3" | jq -r '.balance')
EXPECTED_BAL3=$((DEPOSIT_USER3 - 200000000))  # 700M - 200M = 500M
assert_eq "User3 balance after sub" "$EXPECTED_BAL3" "$BAL3_AFTER"

echo ""

# ───────────────────────────────────────────────────────────────────
# 5. Test subscription cancellation (auto-renew)
# ───────────────────────────────────────────────────────────────────

echo "── Phase 5: Cancel auto-renew for User2 ──"

RESULT=$(send_cancel_subscription "$USER2")
assert_tx "$RESULT" "User2 cancels auto-renew"

wait_blocks 1

SUB2=$(query_subscription "$USER2")
SUB2_AUTO=$(echo "$SUB2" | jq -r '.auto_renew')
assert_eq "User2 auto-renew cancelled" "false" "$SUB2_AUTO"

# Subscription should still be active (plan unchanged)
SUB2_PLAN=$(echo "$SUB2" | jq -r '.plan')
assert_eq "User2 still on Pro plan" "pro" "$SUB2_PLAN"

echo ""

# ───────────────────────────────────────────────────────────────────
# 6. Test additional deposits (top-up)
# ───────────────────────────────────────────────────────────────────

echo "── Phase 6: Top-up deposits ──"

TOPUP=100000000  # 100 BIGT
RESULT=$(send_deposit "$USER1" "$TOPUP")
assert_tx "$RESULT" "User1 top-up ${TOPUP} uBIGT"

wait_blocks 1

ACCT1=$(query_account "$USER1")
BAL1_TOPUP=$(echo "$ACCT1" | jq -r '.balance')
EXPECTED_BAL1_TOPUP=$((EXPECTED_BAL1 + TOPUP))
assert_eq "User1 balance after top-up" "$EXPECTED_BAL1_TOPUP" "$BAL1_TOPUP"

echo ""

# ───────────────────────────────────────────────────────────────────
# 7. Test duplicate subscription (upgrade/extend)
# ───────────────────────────────────────────────────────────────────

echo "── Phase 7: User1 renews subscription (extend) ──"

# Use auto_renew=false to make the tx bytes differ from the first subscribe
RESULT=$(send_subscribe "$USER1" "basic" false)
assert_tx "$RESULT" "User1 renews Basic plan (extends)"

wait_blocks 2

# Balance should be further deducted by 10M (basic plan fee).
ACCT1=$(query_account "$USER1")
BAL1_RENEWED=$(echo "$ACCT1" | jq -r '.balance')
EXPECTED_BAL1_RENEWED=$((EXPECTED_BAL1_TOPUP - 10000000))
assert_eq "User1 balance after renewal" "$EXPECTED_BAL1_RENEWED" "$BAL1_RENEWED"

echo ""

# ───────────────────────────────────────────────────────────────────
# 8. Test insufficient balance (should fail)
# ───────────────────────────────────────────────────────────────────

echo "── Phase 8: Negative tests ──"

# Test 1: Subscribe without an account (should fail with "not found" or non-zero code)
RESULT=$(send_subscribe "user_no_funds" "basic" false 2>/dev/null || true)
TESTS=$((TESTS + 1))
NEG_CODE=$(echo "$RESULT" | jq -r '.result.deliver_tx.code // 0' 2>/dev/null)
NEG_LOG=$(echo "$RESULT" | jq -r '.result.deliver_tx.log // ""' 2>/dev/null)
if [ "$NEG_CODE" != "0" ] && [ "$NEG_CODE" != "null" ]; then
  PASS=$((PASS + 1))
  echo "  ✓ Subscribe without account rejected (code=$NEG_CODE)"
elif echo "$NEG_LOG" | grep -qi "not found\|insufficient\|error"; then
  PASS=$((PASS + 1))
  echo "  ✓ Subscribe without account rejected (log)"
elif echo "$RESULT" | grep -qi "not found\|insufficient\|error"; then
  PASS=$((PASS + 1))
  echo "  ✓ Subscribe without account rejected (raw)"
else
  FAIL=$((FAIL + 1))
  echo "  ✗ Subscribe without account should have been rejected (code=$NEG_CODE)"
fi

# Test 2: Deposit tiny amount, try enterprise plan (should fail with insufficient)
send_deposit "user_poor" 1000 >/dev/null 2>&1 || true
wait_blocks 1
RESULT=$(send_subscribe "user_poor" "enterprise" false 2>/dev/null || true)
TESTS=$((TESTS + 1))
NEG_CODE=$(echo "$RESULT" | jq -r '.result.deliver_tx.code // 0' 2>/dev/null)
NEG_LOG=$(echo "$RESULT" | jq -r '.result.deliver_tx.log // ""' 2>/dev/null)
if [ "$NEG_CODE" != "0" ] && [ "$NEG_CODE" != "null" ]; then
  PASS=$((PASS + 1))
  echo "  ✓ Enterprise with insufficient funds rejected (code=$NEG_CODE)"
elif echo "$NEG_LOG" | grep -qi "insufficient\|error"; then
  PASS=$((PASS + 1))
  echo "  ✓ Enterprise with insufficient funds rejected (log)"
elif echo "$RESULT" | grep -qi "insufficient\|error"; then
  PASS=$((PASS + 1))
  echo "  ✓ Enterprise with insufficient funds rejected (raw)"
else
  FAIL=$((FAIL + 1))
  echo "  ✗ Enterprise with insufficient funds should have been rejected (code=$NEG_CODE)"
fi

echo ""

# ───────────────────────────────────────────────────────────────────
# 9. Multi-validator query check
# ───────────────────────────────────────────────────────────────────

echo "── Phase 9: Cross-validator state consistency ──"

# Get User1's balance from validator1 as the reference.
REF_ACCT=$(query_account "$USER1" 2>/dev/null)
REF_BAL=$(echo "$REF_ACCT" | jq -r '.balance')

# Query User1's account from all 3 validators to verify state sync.
for port in 26657 26658 26659; do
  CROSS_ACCT=$(BIGT_RPC="http://localhost:${port}" query_account "$USER1" 2>/dev/null || echo "")
  if [ -n "$CROSS_ACCT" ]; then
    CROSS_BAL=$(echo "$CROSS_ACCT" | jq -r '.balance')
    assert_eq "User1 balance on port ${port}" "$REF_BAL" "$CROSS_BAL"
  else
    TESTS=$((TESTS + 1))
    FAIL=$((FAIL + 1))
    echo "  ✗ Could not query User1 from port ${port}"
  fi
done

# Query User3's subscription from validator2
CROSS_SUB=$(BIGT_RPC="http://localhost:26658" query_subscription "$USER3" 2>/dev/null || echo "")
if [ -n "$CROSS_SUB" ]; then
  CROSS_PLAN=$(echo "$CROSS_SUB" | jq -r '.plan')
  assert_eq "User3 plan on validator2" "enterprise" "$CROSS_PLAN"
fi

echo ""

# ───────────────────────────────────────────────────────────────────
# Summary
# ───────────────────────────────────────────────────────────────────

echo "============================================"
echo " Test Results: ${PASS}/${TESTS} passed, ${FAIL} failed"
echo "============================================"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi

echo ""
echo "All tests passed!"
exit 0
