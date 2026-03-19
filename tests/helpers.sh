#!/usr/bin/env bash
# helpers.sh — Utility functions for BIGT Docker integration tests.
set -euo pipefail

RPC_URL="${BIGT_RPC:-http://localhost:26657}"

# broadcast_tx sends a JSON transaction via broadcast_tx_sync and returns the result.
# Usage: broadcast_tx '{"type":"deposit","payload":{...},"sender":"addr"}'
broadcast_tx() {
  local tx_json="$1"
  local tx_b64
  tx_b64=$(echo -n "$tx_json" | base64 -w0)
  local result
  result=$(curl -sf -X POST -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"broadcast_tx_sync\",\"params\":{\"tx\":\"${tx_b64}\"},\"id\":1}" \
    "${RPC_URL}" 2>/dev/null)
  echo "$result"
}

# broadcast_tx_commit sends a tx and waits for it to be included in a block.
# Usage: broadcast_tx_commit '{"type":"deposit","payload":{...},"sender":"addr"}'
broadcast_tx_commit() {
  local tx_json="$1"
  local tx_b64
  tx_b64=$(echo -n "$tx_json" | base64 -w0)
  local result
  result=$(curl -sf -X POST -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"broadcast_tx_commit\",\"params\":{\"tx\":\"${tx_b64}\"},\"id\":1}" \
    "${RPC_URL}" 2>/dev/null)
  echo "$result"
}

# abci_query sends an ABCI query and returns the response value (base64-decoded).
# Usage: abci_query "/account" "user1_addr"
abci_query() {
  local path="$1"
  local data="$2"
  local data_hex
  data_hex=$(echo -n "$data" | xxd -p | tr -d '\n')
  local result
  result=$(curl -sf -X POST -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"abci_query\",\"params\":{\"path\":\"${path}\",\"data\":\"${data_hex}\"},\"id\":1}" \
    "${RPC_URL}" 2>/dev/null)

  local code
  code=$(echo "$result" | jq -r '.result.response.code // 0')
  if [ "$code" != "0" ]; then
    local log
    log=$(echo "$result" | jq -r '.result.response.log // "unknown error"')
    echo "QUERY_ERROR: code=$code log=$log" >&2
    return 1
  fi

  local value
  value=$(echo "$result" | jq -r '.result.response.value // empty')
  if [ -n "$value" ]; then
    echo "$value" | base64 -d
  fi
}

# wait_for_chain waits until the chain starts producing blocks.
# Usage: wait_for_chain [max_seconds]
wait_for_chain() {
  local max_wait="${1:-60}"
  local elapsed=0
  echo -n "Waiting for chain to start"
  while [ "$elapsed" -lt "$max_wait" ]; do
    local status
    status=$(curl -sf "${RPC_URL}/status" 2>/dev/null || true)
    if [ -n "$status" ]; then
      local height
      height=$(echo "$status" | jq -r '.result.sync_info.latest_block_height // "0"')
      if [ "$height" != "0" ] && [ "$height" != "null" ]; then
        echo " OK (height=$height)"
        return 0
      fi
    fi
    echo -n "."
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo " TIMEOUT after ${max_wait}s"
  return 1
}

# wait_blocks waits for N new blocks to be produced.
# Usage: wait_blocks [n]
wait_blocks() {
  local n="${1:-1}"
  local status
  status=$(curl -sf "${RPC_URL}/status" 2>/dev/null)
  local start_height
  start_height=$(echo "$status" | jq -r '.result.sync_info.latest_block_height')
  local target=$((start_height + n))
  local max_wait=$((n * 10 + 10))
  local elapsed=0
  while [ "$elapsed" -lt "$max_wait" ]; do
    status=$(curl -sf "${RPC_URL}/status" 2>/dev/null)
    local current
    current=$(echo "$status" | jq -r '.result.sync_info.latest_block_height')
    if [ "$current" -ge "$target" ] 2>/dev/null; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "TIMEOUT waiting for $n blocks" >&2
  return 1
}

# send_deposit sends a deposit transaction for a user.
# Usage: send_deposit "user_addr" amount_ubigt
send_deposit() {
  local addr="$1"
  local amount="$2"
  local payload
  payload=$(jq -nc --arg addr "$addr" --argjson amt "$amount" \
    '{user_addr: $addr, amount: $amt}')
  local tx
  tx=$(jq -nc --arg type "deposit" --argjson payload "$payload" --arg sender "$addr" \
    '{type: $type, payload: $payload, sender: $sender}')
  broadcast_tx_commit "$tx"
}

# send_subscribe sends a subscribe transaction for a user.
# Usage: send_subscribe "user_addr" "plan" [auto_renew=true]
send_subscribe() {
  local addr="$1"
  local plan="$2"
  local auto_renew="${3:-true}"
  local payload
  payload=$(jq -nc --arg addr "$addr" --arg plan "$plan" --argjson ar "$auto_renew" \
    '{user_addr: $addr, plan: $plan, auto_renew: $ar}')
  local tx
  tx=$(jq -nc --arg type "subscribe" --argjson payload "$payload" --arg sender "$addr" \
    '{type: $type, payload: $payload, sender: $sender}')
  broadcast_tx_commit "$tx"
}

# send_cancel_subscription sends a cancel subscription transaction.
# Usage: send_cancel_subscription "user_addr"
send_cancel_subscription() {
  local addr="$1"
  local payload
  payload=$(jq -nc --arg addr "$addr" '{user_addr: $addr}')
  local tx
  tx=$(jq -nc --arg type "cancel_subscription" --argjson payload "$payload" --arg sender "$addr" \
    '{type: $type, payload: $payload, sender: $sender}')
  broadcast_tx_commit "$tx"
}

# query_account queries a user's account balance.
# Usage: query_account "user_addr"
query_account() {
  abci_query "/account" "$1"
}

# query_subscription queries a user's subscription.
# Usage: query_subscription "user_addr"
query_subscription() {
  abci_query "/subscription" "$1"
}

# check_tx_success verifies a broadcast_tx_commit result has code 0.
# Usage: result=$(broadcast_tx_commit ...); check_tx_success "$result" "description"
check_tx_success() {
  local result="$1"
  local desc="${2:-tx}"
  # Check for JSON-RPC error
  local rpc_err
  rpc_err=$(echo "$result" | jq -r '.error.message // empty' 2>/dev/null)
  if [ -n "$rpc_err" ]; then
    echo "FAIL: $desc — RPC error: $rpc_err"
    return 1
  fi
  local code
  code=$(echo "$result" | jq -r '.result.tx_result.code // .result.deliver_tx.code // .result.check_tx.code // 0')
  if [ "$code" != "0" ]; then
    local log
    log=$(echo "$result" | jq -r '.result.tx_result.log // .result.deliver_tx.log // .result.check_tx.log // "unknown"')
    echo "FAIL: $desc — code=$code log=$log"
    return 1
  fi
  echo "OK: $desc"
  return 0
}
