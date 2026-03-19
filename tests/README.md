# BIGT Docker Integration Test

End-to-end test of a **3-validator BIGT network** with **3 users** subscribing
to monthly plans and making payments.

## What it tests

| Phase | Description |
|-------|-------------|
| 1 | Build & start 3 Docker-based CometBFT validator nodes |
| 2 | Verify all 3 validators are running and producing blocks |
| 3 | Deposit uBIGT for 3 user accounts |
| 4 | Subscribe: User1→Basic, User2→Pro, User3→Enterprise |
| 5 | Cancel auto-renew for User2 |
| 6 | Top-up deposit for User1 |
| 7 | Renew/extend subscription for User1 |
| 8 | Negative tests (no account, insufficient funds) |
| 9 | Cross-validator state consistency checks |

## Users & Plans

| User | Plan | Monthly Fee | Job Limit | Auto-Renew |
|------|------|-------------|-----------|------------|
| user1 | Basic | 10 BIGT (10M uBIGT) | 100/month | Yes |
| user2 | Pro | 50 BIGT (50M uBIGT) | 1,000/month | Yes → cancelled |
| user3 | Enterprise | 200 BIGT (200M uBIGT) | Unlimited | No |

## Prerequisites

- Docker & Docker Compose v2
- `curl`, `jq`, `base64`, `xxd` on the host

## Usage

```bash
# Run the full test (build + start + test + teardown)
./docker/test/run_test.sh

# Skip the Docker build step (if images are already built)
SKIP_BUILD=1 ./docker/test/run_test.sh

# Leave containers running after test (for debugging)
SKIP_CLEANUP=1 ./docker/test/run_test.sh

# Use a custom RPC endpoint
BIGT_RPC=http://localhost:26658 ./docker/test/run_test.sh
```

## Manual interaction

```bash
# Start the network
docker compose -f docker/docker-compose.yml up -d --build

# Wait for it
curl -sf http://localhost:26657/status | jq .result.sync_info.latest_block_height

# Send a deposit (50 BIGT to user1)
source docker/test/helpers.sh
send_deposit "user1_test" 50000000

# Query balance
query_account "user1_test"

# Tear down
docker compose -f docker/docker-compose.yml down -v
```
