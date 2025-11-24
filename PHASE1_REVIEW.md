# Phase 1 Review: Multi-Provider Consensus Engine

## Overview
This review covers the implementation of Phase 1 from `shovel_changes.md`: Multi-Provider Consensus Engine. The goal is to ensure all providers agree on log data before advancing task progress.

## Summary of Changes
- Added consensus configuration to sources
- Implemented `ConsensusEngine` with quorum-based fetching
- Added deterministic block hashing for consensus verification
- Integrated consensus into task loading flow
- Added Prometheus metrics for consensus monitoring
- Created database schema for block verification tracking
- Modified eth.Log to include necessary fields for hashing

## Critical Issues Found

### 🔴 CRITICAL: Missing Provider Set Tracking
**Status:** ✅ Completed
**File**: [`shovel/task.go:484-485`](shovel/task.go#L484-L485)

**Current Code:**
```go
// TODO: Store actual provider set IDs instead of dummy value
providerSet := []byte(`["all"]`)
```

**Context:** This appears in the `Converge()` method within the block verification insert loop at lines 486-498.

**Issue**: The implementation uses a placeholder `["all"]` instead of tracking which specific providers participated in consensus. This makes it impossible to audit which providers were used for each block, defeating a key goal of the reconciliation strategy.

**Impact**: HIGH - Cannot track provider reliability or debug consensus failures per provider.

**Similar Pattern in Codebase:** The consensus engine at [`shovel/consensus.go:66-77`](shovel/consensus.go#L66-L77) has access to all providers via `ce.providers`, each with `.NextURL()` method to identify them.

**Recommendation**: Store actual provider URLs/identifiers that participated in consensus:
```go
providerSet := make([]string, len(ce.providers))
for i, p := range ce.providers {
    providerSet[i] = p.NextURL().String()
}
providerSetJSON, _ := json.Marshal(providerSet)
```

---

### 🔴 CRITICAL: Consensus Hash Not Validated on Reorg
**Status:** ✅ Completed
**File**: [`shovel/task.go:428-434`](shovel/task.go#L428-L434)

**Current Code:**
```go
if task.consensus != nil {
    // Consensus fetch (all providers)
    blocks, consensusHash, err = task.consensus.FetchWithQuorum(ctx, &task.filter, localNum+1, delta)
} else {
    // Legacy single-provider fetch
    blocks, err = task.load(ctx, url, localHash, localNum+1, delta)
}
```

**Issue**: The `consensusHash` is computed but never validated against the previous local hash to detect reorgs. The legacy path checks for reorgs in `task.load()`, but the consensus path skips this critical check.

**Impact**: HIGH - Reorgs may go undetected when consensus is enabled, leading to incorrect chain state.

**Comparison with Legacy Path:** [`shovel/task.go:557-558`](shovel/task.go#L557-L558)
```go
first, last := blocks[0], blocks[len(blocks)-1]
if len(first.Header.Parent) == 32 && !bytes.Equal(localHash, first.Header.Parent) {
    return nil, ErrReorg
}
```

The legacy `task.load()` method performs parent hash validation, but the consensus path at line 430 does not.

**Recommendation**: Add reorg detection similar to the legacy path. You need to fetch the parent block hash and compare:
```go
if task.consensus != nil {
    blocks, consensusHash, err = task.consensus.FetchWithQuorum(ctx, &task.filter, localNum+1, delta)
    if err == nil && len(blocks) > 0 {
        // Verify parent hash matches to detect reorgs
        if !bytes.Equal(blocks[0].Parent, localHash) {
            err = ErrReorg
        }
    }
}
```

---

### 🟡 HIGH: Incomplete Log Hashing
**Status:** ✅ Completed
**File**: [`shovel/consensus.go:160-170`](shovel/consensus.go#L160-L170)

**Current Code:**
```go
var buf bytes.Buffer
for _, l := range logs {
    buf.Write([]byte(eth.EncodeUint64(uint64(l.BlockNumber))))
    buf.Write(l.TxHash)
    buf.Write([]byte(eth.EncodeUint64(uint64(l.Idx))))
    buf.Write([]byte(eth.EncodeUint64(uint64(l.Index)))) // ABI index
    buf.Write(l.Data)
    for _, t := range l.Topics {
        buf.Write(t)
    }
}
```

**Issue**: The hash doesn't include the log's `Address` field, which is a critical part of a log's identity. If two logs have identical topics, data, and indices but different addresses, they would hash to the same value.

**Impact**: MEDIUM - Could cause false consensus on distinct logs from different contracts.

**Log Structure Reference:** Per [`eth/types.go:141-150`](eth/types.go#L141-L150), the `Log` struct includes:
```go
type Log struct {
    BlockNumber Uint64  `json:"blockNumber"`
    TxHash      Bytes   `json:"transactionHash"`
    Idx         Uint64  `json:"logIndex"`
    Index       Uint64  `json:"transactionIndex"`
    Address     Bytes   `json:"address"`  // ← Not included in hash!
    Topics      []Bytes `json:"topics"`
    Data        Bytes   `json:"data"`
}
```

**Recommendation**: Include the address:
```go
buf.Write(l.Address)  // Add this first
buf.Write([]byte(eth.EncodeUint64(uint64(l.BlockNumber))))
buf.Write(l.TxHash)
// ... rest of fields
```

---

### 🟡 HIGH: No Validation That Providers Meet Minimum
**Status:** ✅ Completed
**File**: [`shovel/config/config.go:435-437`](shovel/config/config.go#L435-L437)

**Current Code:**
```go
if s.Consensus.Threshold > s.Consensus.Providers {
    return fmt.Errorf("consensus threshold (%d) cannot exceed providers (%d)", s.Consensus.Threshold, s.Consensus.Providers)
}
```

**Issue**: The config validation checks that threshold ≤ providers, but doesn't verify that the actual number of provider URLs matches the configured `Consensus.Providers` value.

**Impact**: MEDIUM - Configuration could specify `providers: 3, threshold: 2` but only provide 1 URL, causing runtime failures.

**Context:** The validation occurs after URLs are collected at [`shovel/config/config.go:392-403`](shovel/config/config.go#L392-L403):
```go
var urls []string
urls = append(urls, string(x.URL))
for _, url := range x.URLs {
    urls = append(urls, string(url))
}

for _, u := range urls {
    if len(u) == 0 {
        continue
    }
    s.URLs = append(s.URLs, u)
}
```

But there's no check that `len(s.URLs) >= s.Consensus.Providers` after this.

**Recommendation**: Add validation:
```go
if len(s.URLs) < s.Consensus.Providers {
    return fmt.Errorf("configured %d consensus providers but only %d URLs provided", s.Consensus.Providers, len(s.URLs))
}
```

---

### 🟡 MEDIUM: Infinite Retry Without Circuit Breaker
**Status:** ✅ Completed
**File**: [`shovel/consensus.go:46-125`](shovel/consensus.go#L46-L125)

**Current Code:**
```go
for attempt := 0; ; attempt++ {  // ← Infinite loop
    // Backoff if this is a retry
    if attempt > 0 {
        delay := conf.RetryBackoff * time.Duration(1<<(attempt-1))
        if delay > conf.MaxBackoff {
            delay = conf.MaxBackoff
        }
        select {
        case <-ctx.Done():
            return nil, nil, ctx.Err()
        case <-time.After(delay):
        }
    }

    // ... fetching and consensus logic ...

    if canonIdx >= 0 {
        // success
        return responses[canonIdx], []byte(canonHash), nil
    }

    ce.metrics.Failure()
    slog.WarnContext(ctx, "consensus-failed",
        "n", start,
        "threshold", conf.Threshold,
        "votes", fmt.Sprintf("%v", counts),
        "attempt", attempt+1,
    )

    // Retry loop continues indefinitely
}
```

**Issue**: The consensus fetch will retry forever if providers never reach consensus. Per the plan (line 139): "All providers disagree: Keep retrying indefinitely until consensus is achieved." However, there's no circuit breaker or maximum retry limit to prevent resource exhaustion.

**Impact**: MEDIUM - Could cause task to hang indefinitely, consuming resources without progress.

**Recommendation**: Add configurable max retries or timeout:
```go
maxRetries := 1000 // or from config
for attempt := 0; attempt < maxRetries; attempt++ {
    // ... existing logic
}
return nil, nil, fmt.Errorf("consensus not reached after %d attempts", maxRetries)
```

Or add a context deadline check in the loop.

---

### 🟡 MEDIUM: Missing Block Number Range in Consensus Hash
**Status:** ✅ Completed
**File**: [`shovel/task.go:486-492`](shovel/task.go#L486-L492)

**Current Code:**
```go
for _, b := range blocks {
    _, err := pgtx.Exec(ctx, q,
        task.srcName,
        task.destConfig.Name,
        b.Num(),           // ← Different block number
        consensusHash,     // ← Same hash for all blocks
        providerSet,
    )
```

**Issue**: The same `consensusHash` is written for every block in the batch, but the hash was computed across ALL blocks in the batch at line 430:
```go
blocks, consensusHash, err = task.consensus.FetchWithQuorum(ctx, &task.filter, localNum+1, delta)
```

This means each block gets the same hash, but that hash represents the entire batch, not the individual block.

**Impact**: MEDIUM - Audit verification in later phases will fail because block hashes don't match actual block content.

**Recommendation**: Either:
1. Compute hash per block: `HashBlocks([]eth.Block{b})`
2. Or store the batch range and only insert one verification row for the entire batch

**Suggested fix**:
```go
// Store one verification row for the entire batch
lastBlock := blocks[len(blocks)-1]
_, err := pgtx.Exec(ctx, q,
    task.srcName,
    task.destConfig.Name,
    lastBlock.Num(),
    consensusHash,
    providerSet,
)
```

---

### 🟢 MINOR: Empty Block Edge Case
**Status:** ✅ Completed
**File**: [`shovel/consensus.go:132-135`](shovel/consensus.go#L132-L135)

**Current Code:**
```go
func HashBlocks(blocks []eth.Block) []byte {
    if len(blocks) == 0 {
        return eth.Keccak([]byte("empty"))
    }
```

**Issue**: Returns a static hash for empty blocks. If two different block ranges both have no matching logs, they'll hash to the same value. This could cause false positives in verification.

**Impact**: LOW - Unlikely in practice since most blocks have some activity.

**Recommendation**: Include the block range in the empty hash:
```go
if len(blocks) == 0 {
    return eth.Keccak([]byte(fmt.Sprintf("empty-range-%d", /* start block num */)))
}
```

---

### 🟢 MINOR: Provider Error Logging in Metrics
**Status:** ✅ Completed
**File**: [`shovel/metrics.go:54-57`](shovel/metrics.go#L54-L57)

**Current Code:**
```go
func (m *Metrics) ProviderError(p string) {
    ProviderErrors.WithLabelValues(p).Inc()
    slog.Error("provider error", "p", p)  // ← ERROR level for expected failures
}
```

**Issue**: Logs every provider error at ERROR level. In a multi-provider setup, transient errors are expected and this will create excessive noise.

**Impact**: LOW - Operational noise, not functional impact.

**Context**: This is called from [`shovel/consensus.go:74`](shovel/consensus.go#L74) when any provider fails:
```go
if err != nil {
    errs[i] = err
    ce.metrics.ProviderError(p.NextURL().String())
    return nil // Don't fail the group, we handle errors individually
}
```

Since the consensus engine continues with remaining providers, individual provider failures are expected operational events, not critical errors.

**Recommendation**: Log at WARN or DEBUG level:
```go
slog.Warn("provider error", "provider", p)
```

---

### 🟢 MINOR: Missing Context Cancellation Check
**Status:** ✅ Completed
**File**: [`shovel/consensus.go:68-79`](shovel/consensus.go#L68-L79)

**Current Code:**
```go
for i, p := range ce.providers {
    i, p := i, p
    eg.Go(func() error {
        blocks, err := p.Get(ctx, p.NextURL().String(), filter, start, limit)
        if err != nil {
            errs[i] = err
            ce.metrics.ProviderError(p.NextURL().String())
            return nil
        }
        responses[i] = blocks
        return nil
    })
}
```

**Issue**: The errgroup doesn't check if context is cancelled before starting fetches. If context is cancelled during the backoff sleep (lines 48-57), the goroutines will still start.

**Impact**: LOW - Slight resource waste on shutdown.

**Recommendation**: Check context before starting the parallel fetch loop:
```go
// Before line 66
if ctx.Err() != nil {
    return nil, nil, ctx.Err()
}

// Parallel fetch
for i, p := range ce.providers {
    // ...
}
```

---

## Missing Functionality from Plan

### 📋 Filter Application in Consensus
**Plan Reference**: Phase 1, line 91: "Modify `shovel/task.load()` to fan-out `eth_getLogs` across configured providers"

**Current Implementation**: `ConsensusEngine.FetchWithQuorum` calls `p.Get(ctx, url, filter, start, limit)` but the filter is passed directly to each provider. There's no verification that the filter was applied correctly by each provider.

**Gap**: No validation that providers applied the same filter or returned logs matching the filter criteria.

**Recommendation**: Add filter validation in `HashBlocks` to ensure all logs match the filter before hashing (or document that this is provider responsibility).

---

### 📋 Block Batching Strategy
**Plan Reference**: Phase 1 - quorum evaluator should handle batches

**Current Implementation**: The consensus engine fetches a batch of blocks and computes one hash for the entire batch.

**Gap**: The plan mentions "per-block checkpoints via task_updates" (line 13), but the implementation stores one consensus hash for multiple blocks. This makes it unclear which specific block a consensus hash corresponds to.

**Recommendation**: Clarify batching strategy - either hash per block or explicitly document that verification is per-batch.

---

## Positive Findings ✅

1. **Good Configuration Design**: The `Consensus` struct with defaults (lines 385-389 in config.go) provides sensible fallbacks.

2. **Deterministic Sorting**: The `HashBlocks` function correctly sorts logs before hashing (lines 146-157).

3. **Proper Metrics Integration**: Prometheus metrics are properly initialized and used throughout (metrics.go).

4. **Transactional Safety**: The task converge logic properly commits both data inserts and verification records in the same transaction (lines 467-502 in task.go).

5. **Backward Compatibility**: The consensus engine is optional - tasks without consensus configured use the legacy path (line 428-434).

6. **Error Handling**: Provider errors don't fail the entire consensus attempt; the engine continues with remaining providers (line 75).

7. **Schema Design**: The `block_verification` table has proper indexing for audit queries (schema.sql:50-51).

---

## Testing Gaps

### Missing Tests:
1. **No integration test** for consensus engine with multiple providers
2. **No test** for reorg detection with consensus enabled  
3. **No test** for partial provider failures (e.g., 2 of 3 succeed)
4. **No test** for configuration validation with mismatched URLs vs Providers count
5. **No test** for batch consensus hash verification
6. **No test** for metrics emission

### Recommended Test Cases:
```go
// Test consensus with provider disagreement
func TestConsensusEngine_Disagreement(t *testing.T) {
    // 3 providers, 2 agree, 1 differs
    // Verify quorum reached with 2-of-3
}

// Test consensus with all providers failing
func TestConsensusEngine_AllFail(t *testing.T) {
    // All providers return errors
    // Verify retry logic with backoff
}

// Test reorg detection with consensus
func TestTask_Converge_ConsensusReorg(t *testing.T) {
    // Setup: localHash doesn't match parent of first block
    // Verify ErrReorg returned
}
```

### Next Steps for Phase 1 (Tests)
Implement the above test cases using existing patterns:

1. Add `TestConsensusEngine_Disagreement` and `TestConsensusEngine_AllFail` to `shovel/consensus_test.go`, mirroring the style of `TestConsensusEngine_New` and `TestHashBlocks_Deterministic` and reusing the `mockClient` helper.
2. Add `TestTask_Converge_ConsensusReorg` in a new consensus-aware converge test, using mock providers similar to `mockClient` to simulate a mismatched parent hash and assert `ErrReorg`.
3. Extend `shovel/shovel/config/config_test.go` with a case that feeds a source config where `consensus.providers` exceeds the number of URLs and asserts that `ValidateFix`/`UnmarshalJSON` returns a descriptive error.

---

## Schema Review

### ✅ Good:
- `block_verification` table has proper unique constraint (src_name, ig_name, block_num)
- Index on (audit_status, block_num) supports audit loop queries
- `retry_count` tracks failed verification attempts
- Timestamps track verification history

### ⚠️ Issues:
- `provider_set` is JSONB but stored as dummy `["all"]` - needs actual data
- No foreign key constraint to `task_updates` (acceptable for perf reasons)
- `consensus_hash` stored for every block in batch but hash is computed for entire batch

---

## Configuration Review

### ✅ Good:
- Defaults are sensible (threshold=1, providers=1, retry=2s, max=30s)
- Duration parsing handles both numeric and string formats
- Validation prevents threshold > providers

### ⚠️ Issues:
- No validation that len(URLs) >= Consensus.Providers
- No validation that Providers > 1 when Threshold > 1
- Retry backoff could overflow duration with many attempts (cap is correct at 30s)

---

## Performance Considerations

1. **Parallel Provider Fetches**: ✅ Good use of errgroup for parallel fetches
2. **Single Hash per Batch**: ✅ Efficient, but needs clarification on verification granularity
3. **In-Memory Log Collection**: ⚠️ Could consume significant memory for large batches with many logs
4. **Retry Backoff**: ✅ Exponential backoff is appropriate

---

## Security Considerations

1. **SQL Injection**: ✅ All queries use parameterized statements
2. **Provider Trust**: ✅ Consensus model assumes providers can be Byzantine
3. **Hash Collisions**: ✅ Keccak256 provides sufficient collision resistance
4. **Advisory Locks**: ✅ Properly used to prevent concurrent task execution (line 159)

---

## Recommendations Summary

### Must Fix Before Phase 2:
1. 🔴 Implement proper provider set tracking
2. 🔴 Add reorg detection for consensus path
3. 🟡 Include log address in hash computation
4. 🟡 Fix batch vs per-block consensus hash storage
5. 🟡 Add config validation for provider URL count

### Should Fix:
1. Add circuit breaker or max retries to consensus loop
2. Improve provider error logging levels
3. Add comprehensive integration tests

### Nice to Have:
1. Context cancellation checks in provider fetches
2. Empty block hash includes range info
3. Filter validation in consensus

---

## Overall Assessment

**Implementation Quality**: 7/10

**Readiness for Phase 2**: Not Ready ⚠️

The Phase 1 implementation demonstrates good architectural design with proper use of Go concurrency patterns, metrics, and transactional database operations. However, there are **critical gaps** that must be addressed:

1. The consensus hash verification is incomplete (reorg detection missing)
2. Provider tracking is not implemented (placeholder only)
3. The batch hashing strategy conflicts with per-block verification requirements

These issues will block Phase 2 (Receipt Validation) and Phase 3 (Audit Loop) because they depend on accurate consensus hashes and provider tracking.

**Recommendation**: Address the 5 "Must Fix" items above before proceeding to Phase 2.
