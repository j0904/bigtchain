#!/usr/bin/env bash
# generate-testnet.sh — Generates CometBFT config and a shared genesis for N validators.
# Usage: ./scripts/generate-testnet.sh <num_validators> <output_dir>
set -euo pipefail

NUM_VALIDATORS="${1:-3}"
OUTPUT_DIR="${2:-./docker/testnet}"

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

# Generate keys for each validator using CometBFT's built-in tools.
# Since we may not have cometbft installed, we produce deterministic key stubs
# and a genesis.json that our BIGT app understands.

CHAIN_ID="bigt-docker-test"
TOTAL_SUPPLY=100000000000000  # 100M BIGT in uBIGT
MIN_STAKE=5000000000           # 5,000 BIGT
BOND=10000000000               # 10,000 BIGT per validator
SLOT_SECONDS=6
EPOCH_SLOTS=100  # short epochs for testing

echo "Generating testnet with $NUM_VALIDATORS validators in $OUTPUT_DIR"

# Build validator entries for genesis.
VALIDATORS_JSON="["
for i in $(seq 1 "$NUM_VALIDATORS"); do
  ADDR="val${i}_addr"
  # Deterministic stub keys (real keys come from CometBFT init).
  PUBKEY="val${i}_ed25519_pubkey"
  BLS_PUBKEY="val${i}_bls_pubkey"
  MONIKER="validator-${i}"
  COMMISSION=500  # 5%

  ENTRY=$(cat <<EOF
{
  "address": "$ADDR",
  "pub_key": "$PUBKEY",
  "bls_pub_key": "$BLS_PUBKEY",
  "bond": $BOND,
  "commission_bps": $COMMISSION,
  "moniker": "$MONIKER"
}
EOF
)
  if [ "$i" -gt 1 ]; then
    VALIDATORS_JSON+=","
  fi
  VALIDATORS_JSON+="$ENTRY"
done
VALIDATORS_JSON+="]"

# Write genesis.json.
GENESIS_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
cat > "$OUTPUT_DIR/genesis.json" <<EOF
{
  "chain_id": "$CHAIN_ID",
  "genesis_time": "$GENESIS_TIME",
  "total_supply_ubigt": $TOTAL_SUPPLY,
  "min_stake_ubigt": $MIN_STAKE,
  "slot_seconds": $SLOT_SECONDS,
  "epoch_slots": $EPOCH_SLOTS,
  "validators": $VALIDATORS_JSON
}
EOF

# Create per-validator directories.
for i in $(seq 1 "$NUM_VALIDATORS"); do
  NODE_DIR="$OUTPUT_DIR/node${i}"
  mkdir -p "$NODE_DIR/config" "$NODE_DIR/data"

  # Copy genesis to each node.
  cp "$OUTPUT_DIR/genesis.json" "$NODE_DIR/config/genesis.json"

  echo "  Created $NODE_DIR"
done

echo ""
echo "Testnet generated:"
echo "  Genesis: $OUTPUT_DIR/genesis.json"
echo "  Nodes:   $OUTPUT_DIR/node1 .. node${NUM_VALIDATORS}"
echo ""
echo "Run: docker compose -f docker/docker-compose.yml up --build"
