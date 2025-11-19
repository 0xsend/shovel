# Reconciliation Strategy for Shovel Indexer to Prevent Missing Rows

# [SEN-68](https://linear.app/sendapp/issue/SEN-68/reconciliation-strategy-for-shovel-indexer-to-prevent-missing-rows) – Log Completeness Architecture for Shovel

## Problem Statement

Critical integrations (Send Earn, rewards, compliance reporting) cannot tolerate any missing events once a block has reached the configured confirmation depth. Today a single faulty RPC provider can return incomplete `eth_getLogs` responses, and Shovel will faithfully store that incomplete view. The goal for [SEN-68](https://linear.app/sendapp/issue/SEN-68/reconciliation-strategy-for-shovel-indexer-to-prevent-missing-rows) is to design a Shovel-native strategy that **guarantees every confirmed block is re-verified until all logs are present**, without relying on explorer APIs or external integrations.

## Operating Assumptions

* All integrations are high-value: zero tolerance for missed data.
* We have a large RPC budget, but users must be able to tune the redundancy level per deployment.
* Shovel already provides transactional inserts, reorg handling, and per-block checkpoints via `task_updates`.
* RPC nodes can intermittently omit logs or return empty results while still reporting the block as processed.

## Design Requirements

1. **Immediate Detection:** Do not advance `task_updates` for a block unless at least one provider delivered a complete, validated log set.
2. **Redundant Validation:** Every block at or beyond the confirmation threshold must be re-scanned via an independent pathway until all providers converge.
3. **Automated Remediation:** When inconsistencies are found, the block must be removed and reindexed automatically—no manual backfill workflows.
4. **Budget Controls:** Operators configure how many providers participate in consensus, retry fan-out, and post-confirmation audits.
5. **Observability:** Track per-block completeness state, provider reliability, and automatic recovery attempts.

## Failure Modes to Guard Against

| Failure | Mitigation |
| -- | -- |
| Provider returns zero logs for a block that contains target events | Multi-provider consensus refuses to checkpoint until quorum reached |
| Provider returns subset of logs | Receipt-level verification and post-confirmation audit detect mismatch, trigger re-run |
| Provider lags or temporarily fails | Adaptive retry schedule, quorum logic tolerates `f` faulty nodes |
| Silent bad data after confirmations | Confirmation audit loop replays block until canonical hash matches stored data |

## Strategy Overview

### 1\. Multi-Provider Consensus Pipeline (Real-Time)

* Each Shovel source is configured with **M RPC URLs**. For every block fetched, Shovel issues `eth_getLogs` queries to **K providers in parallel (K ≤ M)**.
* Logs are normalized and hashed (`keccak256(blockNum || txHash || logIdx || abiIdx || payloadHash)`).
* Shovel only advances to the insertion phase when **N-of-K providers** agree on the exact hash set. `N` is tunable (default 2-of-3).
* If consensus fails within the real-time window, Shovel enqueues the block into a retry queue that keeps polling additional providers until consensus is achieved.
* Operators tune `M`, `K`, and `N` to trade RPC cost against risk.

### 2\. Receipt-Level Validation (Hot-Path Safeguard)

* After inserting logs (based on consensus output), Shovel fetches `eth_getBlockReceipts` or `eth_getTransactionReceipt` for the same block from a **different provider class** (e.g., Erigon + Geth mix).
* Receipts are reduced to their log hashes and compared to inserted data. Any divergence triggers:
  1. Removal of the block via existing reorg cleanup (delete rows ≥ block).
  2. Automatic re-fetch using a fresh provider subset.
* Because receipts encode full log data, this step detects partial omissions even if providers agreed on wrong emptiness earlier.

### 3. Confirmation Audit Loop (Post-Processing)

* A dedicated verifier goroutine scans blocks once they reach `confirmations_threshold` (e.g., 128).
* For each confirmed block, it replays the exact filter against **two fresh providers** (round-robin). The resulting hash is compared to the canonical hash stored during ingestion.
* Mismatches enqueue the block for forced reindex with an expanded provider pool (e.g., try all configured providers + fallback archival provider).
* The audit loop writes `shovel.block_verification` rows capturing status: `pending`, `verifying`, `healthy`, `retrying`, `failed`.

### 4. External Repair API (On-Demand)

* Exposes an HTTP API to manually trigger re-verification and re-indexing for specific block ranges.
* Acquires advisory locks to safely delete and re-fetch data without race conditions.
* Supports a "force all providers" mode to exhaustively query every configured RPC endpoint when automatic recovery fails.
* Tracks repair job status and history in a persistent table.

Together these layers create a "belt and suspenders" system: real-time consensus prevents obvious misses, receipt verification catches subtle discrepancies immediately, and the confirmation audit guarantees convergence once blocks are final. The repair API provides a manual escape hatch for catastrophic failures.

## Budget Tuning Levers

| Lever | Effect | Default |
| -- | -- | -- |
| `consensus.providers` | Number of parallel RPC providers per block | 3 |
| `consensus.threshold` | N in N-of-K agreement | 2 |
| `consensus.retry_backoff` | Seconds between provider retries | 2 → capped at 30 |
| `receipt_verifier.provider` | Provider class reserved for receipts | Dedicated archival node |
| `audit.providers_per_block` | Number of providers used during confirmation audits | 2 |
| `audit.confirmations` | Blocks deep before auditing | 128 |
| `audit.parallelism` | Number of concurrent audit workers | 4 |

Deployments with tighter budgets can drop to 2 providers per step; high-assurance deployments can crank up to 4-5 providers and shorter audit intervals.

## Implementation Plan

### Phase 0 – Instrumentation & Schema (0.5 day)

* Add `shovel.block_verification` table capturing block number, provider set, consensus hash, receipt hash, audit status, and retry count.
* Add `shovel.repair_jobs` and `shovel.repair_audit` tables for repair state persistence and logging.
* Extend logging to include provider ID, response fingerprint, and consensus result for every block.

### Phase 1 – Multi-Provider Consensus Engine (2 days)

* Modify `shovel/task.load()` to fan-out `eth_getLogs` across configured providers and collect responses before proceeding.
* Implement deterministic hash builder and quorum evaluator.
* Ensure partial failures push the block into a retry queue without advancing `task_updates`.

### Phase 2 – Receipt Validation (1 day)

* After inserts, fetch receipts from a secondary provider and compare hashed payloads.
* On mismatch, delete block rows and set verification status to `retrying`.

### Phase 3 – Confirmation Audit Loop (2 days)

* New goroutine per Manager that polls `task_updates`, selects blocks older than `audit.confirmations`, and verifies against independent providers.
* On success, mark block as `healthy`. On failure, trigger forced reindex and increment retry counters.

### Phase 4 – External Repair API (2.5 days)

* Implement `RepairService` with `POST /api/v1/repair` and status polling endpoints.
* Add `shovel.repair_jobs` table for persistence and `google/uuid` for IDs.
* Use advisory locks to coordinate repairs and prevent overlaps.
* Integrate with `Task.Delete()` for clean rollback and `Task.load()` for expanded provider querying.
* Add metrics: `shovel_repair_requests_total`, `shovel_repair_blocks_reprocessed`.

### Phase 5 – Observability & Operator Controls (1 day)

* Metrics: consensus failure rate, provider scoring, audit backlog, forced reindex counts.
* CLI / config fields exposing the tuning levers listed above.
* Runbook updates describing how to increase redundancy or pause audits.

### Phase 6 – Hardening & Chaos Exercises (ongoing)

* Simulate providers that omit logs or return zero results to validate automatic recovery.
* Track MTTR for forced reindexes and adjust thresholds.

**Future Work:** After these phases land, evaluate adding per-integration consensus overrides so high-risk integrations can request more redundancy without impacting others.

## Monitoring & Alerts

* **Consensus alert:** triggered if >1% of blocks require more than two retries to reach quorum.
* **Receipt mismatch alert:** any block that fails receipt verification more than once.
* **Audit backlog alert:** confirmed blocks waiting >30 minutes for audit completion.
* **Provider health scoring:** rolling failure rate per provider; auto-demote providers exceeding threshold.

## Open Questions / Decisions

1. **Confirmation depth:** Keep configurable with a default of 128 confirmations. Operators can adjust per deployment.
2. **Per-integration overrides:** Leave as follow-up work. Add `// FUTURE: allow per-integration consensus overrides` comment near the configuration parsing code when implemented.
3. **Recovery pool vs task worker:** Still open; revisit when designing the task worker refactor.
4. **Provider credential distribution:** Out of scope for the Shovel application—handled by deployment tooling.
5. **All providers disagree:** Keep retrying indefinitely until consensus is achieved.

## Prometheus Metrics

Expose the following metrics to observe the new pathways:

| Metric | Type | Description |
| -- | -- | -- |
| `shovel_consensus_attempts_total` | Counter | Number of consensus evaluations per block range. |
| `shovel_consensus_failures_total` | Counter | Consensus attempts that failed to reach threshold. |
| `shovel_consensus_duration_seconds` | Histogram | Time taken to reach consensus for a block. |
| `shovel_receipt_mismatch_total` | Counter | Blocks that failed receipt-level verification. |
| `shovel_forced_reindex_total` | Counter | Forced reindex operations triggered by receipt/audit mismatches. |
| `shovel_audit_verifications_total` | Counter | Confirmation audits executed. |
| `shovel_audit_failures_total` | Counter | Audits that detected divergence. |
| `shovel_audit_queue_length` | Gauge | Number of confirmed blocks pending audit. |
| `shovel_provider_error_total{provider=}` | Counter | RPC errors per provider to inform auto-demotion. |
| `shovel_repair_requests_total` | Counter | Number of manual repair requests. |
| `shovel_repair_blocks_reprocessed` | Counter | Total blocks reprocessed via repair API. |

Alert thresholds mentioned earlier can be driven directly from these metrics.

## Next Steps

1. Align on confirmation depth and default quorum settings.
2. Implement Phase 0–2 and validate on a staging database with synthetic provider faults.
3. Roll out the audit loop, confirm it catches intentionally dropped logs, and tune alert thresholds.
4. Document operator guidance for scaling RPC spend up or down while maintaining the "no missed logs after confirmation" guarantee.

---

## Background & References

### Thread Discussion

[https://send-app.slack.com/archives/C07BGR3DJ8G/p1763053696908979](https://send-app.slack.com/archives/C07BGR3DJ8G/p1763053696908979)

### Related Issues

* SEN-60: Fix balance data in Shovel indexer tables
* PR #2191: Shovel backfill integration

### External Resources

* Shovel Docs: [https://indexsupply.com/shovel/docs/](https://indexsupply.com/shovel/docs/)
* Shovel GitHub: [https://github.com/indexsupply/shovel](https://github.com/indexsupply/shovel)
* Daimo's Experience: [https://daimo.super.site/blog/less-terrible-ethereum-indexing](https://daimo.super.site/blog/less-terrible-ethereum-indexing)

### Known eth_getLogs Issues

* [https://github.com/ethereum/go-ethereum/issues/18198](https://github.com/ethereum/go-ethereum/issues/18198)
* [https://github.com/ethereum/go-ethereum/issues/21770](https://github.com/ethereum/go-ethereum/issues/21770)
* [https://github.com/ethereum/go-ethereum/issues/15936](https://github.com/ethereum/go-ethereum/issues/15936)
* [https://github.com/ethereum/go-ethereum/issues/23875](https://github.com/ethereum/go-ethereum/issues/23875)
* [https://github.com/ethereum/go-ethereum/issues/30516](https://github.com/ethereum/go-ethereum/issues/30516)
* [https://github.com/ethers-io/ethers.js/issues/4703](https://github.com/ethers-io/ethers.js/issues/4703)
* [https://github.com/ethereum/go-ethereum/issues/24136](https://github.com/ethereum/go-ethereum/issues/24136)
* [https://github.com/graphprotocol/graph-node/issues/1130](https://github.com/graphprotocol/graph-node/issues/1130)
* [https://stackoverflow.com/questions/78422492/method-eth-getlogs-in-batch-triggered-rate-limit](https://stackoverflow.com/questions/78422492/method-eth-getlogs-in-batch-triggered-rate-limit)

## Metadata
- URL: [https://linear.app/sendapp/issue/SEN-68/reconciliation-strategy-for-shovel-indexer-to-prevent-missing-rows](https://linear.app/sendapp/issue/SEN-68/reconciliation-strategy-for-shovel-indexer-to-prevent-missing-rows)
- Identifier: SEN-68
- Status: Backlog
- Priority: Urgent
- Assignee: Vic Gin
- Labels: Bug
- Created: 2025-11-14T03:08:02.153Z
- Updated: 2025-11-17T15:00:55.461Z

## Comments

- Allen:

  ## GitHub Issues & References

  Research into `eth_getLogs` reliability issues and industry context:

  ### Geth (go-ethereum) Issues with eth_getLogs

  **Missing logs that appear after node restart:**

  * [Issue #18198 - "missing logs in eth_getLogs"](https://github.com/ethereum/go-ethereum/issues/18198)
    * Logs correctly returned for a time, then one log not returned. Missing log persists until Geth restart, then appears again.

  **Missing logs fixed by resyncing:**

  * [Issue #21770 - "eth_getLogs returns wrong number of logs"](https://github.com/ethereum/go-ethereum/issues/21770)
    * Missing logs for a few blocks. Fixed by deleting datadir and resyncing node.

  **Inconsistent results on repeated queries:**

  * [Issue #15936 - "getLogs returning inconsistent results"](https://github.com/ethereum/go-ethereum/issues/15936)
    * Same query returns varying numbers of results (41, then 11, 9, 53, etc.) on repeated calls.

  **Archive node failures:**

  * [Issue #23875 - "eth_getLogs request returns error 'failed to get logs for block'"](https://github.com/ethereum/go-ethereum/issues/23875)
    * Reproduced on 10 separate archive nodes. Parity/Erigon responded correctly.

  **Recent RLP decoding errors:**

  * [Issue #30516 - "eth_getLogs fails to get logs" (Sep 2024)](https://github.com/ethereum/go-ethereum/issues/30516)
    * RLP decoding error when retrieving logs.

  ### Alchemy-Specific Issues

  **Response size limits:**

  * [Ethers.js Issue #4703 - Error handling with Alchemy/Infura](https://github.com/ethers-io/ethers.js/issues/4703)
    * "Log response size exceeded" - Alchemy limits: 2K block range OR 10K logs max

  **500 error handling:**

  * [Go-Ethereum #24136 - "filterLogs gets stuck on Alchemy 500 error"](https://github.com/ethereum/go-ethereum/issues/24136)
    * When Alchemy returns 500 error, query gets stuck

  **Batch request limits:**

  * [Stack Overflow - "eth_getLogs batch triggered rate limit"](https://stackoverflow.com/questions/78422492/method-eth-getlogs-in-batch-triggered-rate-limit)
    * Alchemy has 500-block limit for batch requests (not single requests)

  ### Graph Protocol / Indexing at Scale

  **Undocumented limits cause timeouts:**

  * [Graph Protocol #1130 - "Optimize historical event scanning"](https://github.com/graphprotocol/graph-node/issues/1130)
    * Undocumented Alchemy limits cause 503 timeouts with many contract addresses

  ### Daimo's Experience

  **Custom indexer built after Shovel issues:**

  * [Blog Post: "Less Terrible Ethereum Indexing"](https://daimo.super.site/blog/less-terrible-ethereum-indexing)
    * "At least one serious reliability issue, cost surprise, or outright correctness bug" with each RPC provider (Alchemy, Quicknode, Chainstack, LlamaRPC)
    * Tried Shovel first but had to build custom indexer for their needs
  * [Daimo GitHub Repo](https://github.com/daimo-eth/daimo)

  ### Shovel Documentation

  **How Shovel handles node reliability:**

  * [Shovel Documentation](https://indexsupply.com/shovel/docs/)
    * Shovel mitigates missing logs by batching `eth_getLogs` + `eth_getBlockByNumber` together and testing the block response to ensure the node has processed the requested block.

  ---

  **Key Takeaway:** This is a well-documented industry-wide issue affecting multiple node implementations (Geth, Erigon, Parity) and RPC providers (Alchemy, Infura, Quicknode, etc.). Root causes vary: bloom filters, receipt storage, RLP encoding, node sync issues, rate limits, and chain reorgs.
