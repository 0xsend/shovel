# Shovel Operator Runbook

This guide describes how to operate Shovel with the Log Completeness / Reconciliation features (SEN-68).

## 1. Tuning Redundancy & RPC Budget

Shovel allows you to trade off RPC cost against data safety using the `eth_sources` configuration.

### Configuration Levers

*   `consensus.providers` (Default: 1): How many providers to query in parallel for every block.
*   `consensus.threshold` (Default: 1): How many providers must agree on the log hash before indexing.
*   `receipt_verifier.enabled` (Default: false): Whether to fetch receipts from a secondary provider to validate logs.
*   `audit.enabled` (Default: false): Whether to re-verify blocks after confirmation depth.

### Scenarios

**Low Cost (Dev/Staging)**
Minimal redundancy. Use for non-critical environments.
```json
"consensus": { "providers": 1, "threshold": 1 },
"receipt_verifier": { "enabled": false },
"audit": { "enabled": false }
```

**High Availability (Production - Standard)**
Protects against single-provider failures.
```json
"consensus": { "providers": 2, "threshold": 2 },
"receipt_verifier": { "enabled": true },
"audit": { "enabled": true, "confirmations": 128 }
```

**Paranoid (Production - Financial/Compliance)**
Maximal assurance. Tolerates multiple provider failures and malicious data.
```json
"consensus": { "providers": 3, "threshold": 2 },
"receipt_verifier": { "enabled": true },
"audit": { "enabled": true, "confirmations": 64, "providers_per_block": 3 }
```

## 2. Managing Audits

The Confirmation Audit Loop runs in the background to verify finalized blocks.

### Pausing Audits
If audits are consuming too many RPC credits or causing load issues, you can disable them without stopping indexing:
1. Update config: set `audit.enabled` to `false`.
2. Restart Shovel.

### Clearing Audit Backlog
If `shovel_audit_queue_length` is growing:
1. Increase `audit.parallelism` (Default: 1).
2. Reduce `audit.confirmations` temporarily (careful: increases risk of reorgs triggering false positives).

## 3. Handling Alerts

### `shovel_consensus_failures_total` Increasing
*   **Cause:** Providers are returning different data for the same block.
*   **Action:** Check `shovel_provider_error_total` to identify a faulty provider. Remove or replace the bad provider in config.

### `shovel_receipt_mismatch_total` > 0
*   **Cause:** The indexed logs did not match the receipt root from the verifier provider.
*   **Action:** Shovel automatically rejects the block. If persistent, investigate if one provider is desynced.

### `shovel_forced_reindex_total` Increasing
*   **Cause:** The Audit loop found a mismatch on an already-indexed confirmed block.
*   **Action:** Shovel automatically deletes and reindexes the block. This indicates a "silent failure" in the real-time path. If frequent, increase `consensus.threshold`.

## 4. Manual Repair (Escape Hatch)

If automatic recovery fails, use the Repair API to force re-verification.

**API Endpoint:** `POST /api/v1/repair`

**Example:**
```bash
curl -X POST http://localhost:8080/api/v1/repair -d '{
  "src_name": "mainnet",
  "ig_name": "seaport",
  "start_block": 18000000,
  "stop_block": 18000100,
  "reason": "manual fix for missing logs"
}'
```

This will:
1. Delete data for the range.
2. Re-fetch using *all* configured providers.
3. Re-index.
