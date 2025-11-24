# Phase 3 Review: Confirmation Audit Loop

## Overview
This review covers the implementation of Phase 3 from `shovel_changes.md`: Confirmation Audit Loop. The goal is to verify blocks once they reach confirmation depth by re-fetching from independent providers.

## Summary of Changes
- Added `Auditor` that polls for blocks needing audit
- Added `Audit` configuration to sources
- Integrated auditor startup with Manager
- Implements parallel verification with configurable providers
- Triggers automatic block deletion and reindex on mismatch
- 226 new lines of code in audit.go

## Critical Issues Found

### 🔴 CRITICAL: Validates Single Block Against Batch Hash
**File**: [`shovel/audit.go:160-177`](shovel/audit.go#L160-L177)

**Current Code:**
```go
for i := 0; i < k; i++ {
    idx := (startIdx + i) % len(providers)
    p := providers[idx]
    wg.Add(1)
    go func(p *jrpc2.Client) {
        defer wg.Done()
        blocks, err := p.Get(ctx, p.NextURL().String(), &task.filter, t.blockNum, 1)  // ← Single block!
        if err != nil {
            slog.WarnContext(ctx, "audit fetch failed", "provider", p.NextURL().Hostname(), "error", err)
            return
        }
        h := HashBlocks(blocks)  // ← Hash of single block
        if bytes.Equal(h, t.consensusHash) {  // ← Compare to batch hash!
            mu.Lock()
            matches++
            mu.Unlock()
        }
    }(p)
}
```

**Context**: The `t.consensusHash` comes from the database query at [`audit.go:70`](shovel/audit.go#L70):
```go
SELECT bv.src_name, bv.ig_name, bv.block_num, bv.consensus_hash
FROM shovel.block_verification bv
```

This hash was stored by Phase 1 at [`task.go:513`](shovel/task.go#L513) from the ENTIRE batch:
```go
consensusHash,  // ← Hash from batch of blocks (computed at line 437)
```

**Issue**: This is the **same critical bug from Phase 2**. The code:
1. Fetches **single block** (t.blockNum, limit 1)
2. Compares its hash against `consensusHash` from database
3. But `consensusHash` was computed from a **batch of blocks** (Phase 1 issue #6)

**Impact**: CRITICAL - Audit will **always fail** or produce false results because single-block hash ≠ batch hash. The entire audit loop is broken.

**Status**: ✅ Fixed – Phase 1 now stores a per-block `consensus_hash` for every block in the batch using `HashBlocks([]eth.Block{b})`, and the audit path still validates a single block via `HashBlocks(blocks)`. The hash stored in `shovel.block_verification` now matches the hash computed during audit for that specific block.

**Recommendation (historical)**: Originally required fixing Phase 1's batch hashing strategy. The implementation chose "Option 1" by storing per-block hashes instead of batch hashes.
```go
// Option 1: Store per-block hashes in Phase 1
// Then audit can validate each block individually

// Option 2: Fetch and validate entire batch in audit
blockStart := t.blockNum // calculate batch start
blockLimit := /* batch size */ 
blocks, err := p.Get(ctx, p.NextURL().String(), &task.filter, blockStart, blockLimit)
h := HashBlocks(blocks)
// Compare against batch hash
```

---

### 🔴 CRITICAL: Inherits All Phase 1 Hash Bugs
**File**: [`shovel/audit.go:171`](shovel/audit.go#L171)

**Current Code:**
```go
h := HashBlocks(blocks)  // ← Uses Phase 1's HashBlocks
if bytes.Equal(h, t.consensusHash) {
    mu.Lock()
    matches++
    mu.Unlock()
}
```

**Phase 1 HashBlocks Implementation**: [`shovel/consensus.go:160-170`](shovel/consensus.go#L160-L170)
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
    // ← MISSING: l.Address is not included!
}
```

**Issue**: Uses the same `HashBlocks` from Phase 1 that has:
1. Missing log address in hash (Phase 1 issue #3)
2. Empty block static hash at [`consensus.go:132-135`](shovel/consensus.go#L132-L135)
3. No block number range in hash

**Impact**: CRITICAL - Even if batching is fixed, audit will produce incorrect hashes and false mismatches/matches.

**Recommendation**: Fix `HashBlocks` in consensus.go first before Phase 3 can function.

**Status**: ✅ Fixed – `HashBlocks` was updated to include the log address, use deterministic sorting, and rely on `HashBlocksWithRange` for empty ranges. Both consensus and audit now share the corrected implementation.

---

### 🔴 CRITICAL: Race Condition with Task.Delete
**File**: [`shovel/audit.go:211-223`](shovel/audit.go#L211-L223)

**Current Code:**
```go
// Trigger Delete/Reindex
// We need a wpg.Conn to pass to Delete. Use pgp.Acquire?
// Task.Delete takes wpg.Conn.
conn, err := a.pgp.Acquire(ctx)  // ← No locking!
if err != nil {
    return fmt.Errorf("acquiring conn for delete: %w", err)
}
defer conn.Release()

// *pgxpool.Conn implements wpg.Conn interface
if err := task.Delete(conn.Conn(), t.blockNum); err != nil {  // ← Concurrent with main task!
    return fmt.Errorf("deleting block for reindex: %w", err)
}

return nil
```

**Context**: The audit goroutine runs concurrently with task goroutines. Task goroutines are started at [`task.go:850-857`](shovel/task.go#L850-L857):
```go
var wg sync.WaitGroup
for i := range tm.tasks {
    i := i
    wg.Add(1)
    go func() {
        tm.runTask(tm.tasks[i])  // ← Task running concurrently
        wg.Done()
    }()
}
```

And auditor is started at [`task.go:840-846`](shovel/task.go#L840-L846):
```go
for _, sc := range tm.conf.Sources {
    if sc.Audit.Enabled {
        tm.auditor = NewAuditor(tm.pgp, tm.conf, tm.tasks)
        go tm.auditor.Run(tm.ctx)  // ← Runs concurrently with tasks
        break
    }
}
```

**Task.Delete Implementation**: The delete removes ALL blocks >= the specified block number, affecting multiple blocks at once.

**Issue**: The audit goroutine calls `task.Delete()` concurrently while the main task goroutine may be processing blocks. This creates a **race condition**:
1. Audit finds block N needs reindex
2. Audit calls `task.Delete(N)` 
3. Meanwhile, main task is inserting block N+1
4. Delete removes blocks ≥ N (including N+1 that was just inserted)
5. Task state becomes corrupted

**Impact**: CRITICAL - Data corruption, lost blocks, database inconsistency.

**Recommendation**: Add coordination between auditor and tasks:
```go
// Option 1: Use advisory locks per task before deletion
lockID := task.lockid // task already has this
acquired, err := pg.Exec(ctx, "SELECT pg_try_advisory_lock($1)", lockID)
if !acquired {
    // Task is running, defer audit
    return nil
}
defer pg.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockID)

// Now safe to delete
if err := task.Delete(conn.Conn(), t.blockNum); err != nil {
    return fmt.Errorf("deleting block for reindex: %w", err)
}

// Option 2: Signal task to delete via channel instead of direct call
```

**Status**: ✅ Fixed – both the main task loop and the auditor now acquire a transaction-scoped advisory lock (`pg_advisory_xact_lock`) keyed by `task.lockid` before mutating `shovel.task_updates` and destination tables. Auditor deletes run inside their own transaction under the same lock, preventing concurrent deletes/inserts for the same (src, ig) pair.

---

### 🟡 HIGH: No Backpressure on Audit Queue
**Files**: [`shovel/audit.go:77`](shovel/audit.go#L77) and [`shovel/audit.go:100`](shovel/audit.go#L100)

**Query Code:**
```go
const q = `
    SELECT bv.src_name, bv.ig_name, bv.block_num, bv.consensus_hash
    FROM shovel.block_verification bv
    JOIN shovel.task_updates tu ON bv.src_name = tu.src_name AND bv.ig_name = tu.ig_name
    WHERE bv.src_name = $1
      AND bv.audit_status IN ('pending', 'retrying')
      AND bv.block_num <= (tu.num - $2)
    ORDER BY bv.block_num ASC
    LIMIT 100  // ← Always fetches up to 100
`
```

**Processing Code:**
```go
// Process batch
// Limit parallelism
sem := make(chan struct{}, sc.Audit.Parallelism)  // ← e.g., 4 workers
var wg sync.WaitGroup
for _, t := range batch {
    wg.Add(1)
    sem <- struct{}{}  // ← Blocks if 4 workers busy
    go func(t auditTask) {
        // ... verification
    }(t)
}
wg.Wait()  // ← Waits for all 100 to complete
```

**Tick Loop**: At [`audit.go:42-53`](shovel/audit.go#L42-L53):
```go
ticker := time.NewTicker(5 * time.Second)  // ← Every 5 seconds
defer ticker.Stop()
for {
    select {
    case <-ctx.Done():
        return
    case <-ticker.C:
        if err := a.check(ctx); err != nil {  // ← Queries 100 more blocks
            slog.ErrorContext(ctx, "audit check", "error", err)
        }
    }
}
```

**Issue**: The audit loop:
1. Queries up to 100 blocks needing audit
2. Processes them with limited parallelism (e.g., 4 workers)
3. Immediately queries next 100 blocks on next tick (5 seconds)

If audit can't keep up (e.g., many blocks failing), the queue grows unbounded and queries get more expensive.

**Impact**: HIGH - Performance degradation, increased database load.

**Recommendation**: Add backpressure:
```go
// Track in-flight audits
type Auditor struct {
    // ...
    inFlight atomic.Int64
    maxInFlight int64
}

func (a *Auditor) check(ctx context.Context) error {
    if a.inFlight.Load() >= a.maxInFlight {
        return nil // skip this tick
    }
    // ... existing logic
}
```

---

### 🟡 HIGH: Fixed 5-Second Tick Interval
**File**: [`shovel/audit.go:42`](shovel/audit.go#L42)

**Current Code:**
```go
func (a *Auditor) Run(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Second)  // ← Hardcoded!
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := a.check(ctx); err != nil {
                slog.ErrorContext(ctx, "audit check", "error", err)
            }
        }
    }
}
```

**Configuration Structure**: At [`config/config.go:361-366`](shovel/config/config.go#L361-L366):
```go
type Audit struct {
    ProvidersPerBlock int    `json:"providers_per_block"`
    Confirmations     uint64 `json:"confirmations"`
    Parallelism       int    `json:"parallelism"`
    Enabled           bool   `json:"enabled"`
    // ← No CheckInterval field!
}
```

**Issue**: Hardcoded 5-second interval is not configurable. For deployments with:
- High block rate → too slow, audit backlog builds
- Low block rate → too frequent, wastes resources

**Impact**: MEDIUM-HIGH - Cannot tune for different deployment scenarios.

**Status**: ✅ Fixed – the `Audit` config now includes a `check_interval` duration, parsed in `config.Source.UnmarshalJSON`, and `Auditor` computes its ticker interval from the smallest configured value across enabled sources (defaulting to 5s).

**Recommendation**: Make configurable:
```go
type Audit struct {
    // ...
    CheckInterval time.Duration `json:"check_interval"`
}

// In Auditor.Run:
interval := 5 * time.Second
if /* configured */ {
    interval = sc.Audit.CheckInterval
}
ticker := time.NewTicker(interval)
```

---

### 🟡 MEDIUM: Query Joins task_updates for Every Block
**File**: [`shovel/audit.go:69-78`](shovel/audit.go#L69-L78)

**Current Code:**
```go
// Query blocks pending audit for this source
const q = `
    SELECT bv.src_name, bv.ig_name, bv.block_num, bv.consensus_hash
    FROM shovel.block_verification bv
    JOIN shovel.task_updates tu ON bv.src_name = tu.src_name AND bv.ig_name = tu.ig_name  // ← JOIN every 5s!
    WHERE bv.src_name = $1
      AND bv.audit_status IN ('pending', 'retrying')
      AND bv.block_num <= (tu.num - $2)  // ← Requires scanning task_updates
    ORDER BY bv.block_num ASC
    LIMIT 100
`
rows, err := a.pgp.Query(ctx, q, sc.Name, sc.Audit.Confirmations)
```

**Context**: This query runs in the tick loop every 5 seconds at [`audit.go:42-53`](shovel/audit.go#L42-L53). With many integrations (100s), the JOIN becomes expensive as it must match `(src_name, ig_name)` pairs.

**Issue**: The JOIN with `task_updates` happens for every audit check (every 5 seconds). The condition `bv.block_num <= (tu.num - $2)` checks if block is old enough, but this requires scanning task_updates.

With many integrations, this JOIN becomes expensive.

**Impact**: MEDIUM - Database performance issue at scale.

**Status**: ✅ Fixed – the audit query now uses a correlated subquery against `MAX(tu.num)` per `(src_name, ig_name)`, avoiding a direct join on every row and aligning with the index on `(ig_name, src_name, num DESC)`.

**Recommendation**: Optimize query:
```go
// Option 1: Use subquery with DISTINCT ON for latest task update
SELECT bv.src_name, bv.ig_name, bv.block_num, bv.consensus_hash
FROM shovel.block_verification bv
WHERE bv.src_name = $1
  AND bv.audit_status IN ('pending', 'retrying')
  AND bv.block_num <= (
      SELECT MAX(tu.num) - $2
      FROM shovel.task_updates tu
      WHERE tu.src_name = bv.src_name AND tu.ig_name = bv.ig_name
  )
ORDER BY bv.block_num ASC
LIMIT 100

// Option 2: Denormalize latest block num into block_verification
```

---

### 🟡 MEDIUM: No Max Retry Limit for Failed Audits
**File**: [`shovel/audit.go:201-209`](shovel/audit.go#L201-L209)

**Current Code:**
```go
// Update status to retrying
const r = `
    UPDATE shovel.block_verification
    SET audit_status = 'retrying', retry_count = retry_count + 1, last_verified_at = now()  // ← Infinite retries!
    WHERE src_name = $1 AND ig_name = $2 AND block_num = $3
`
if _, err := a.pgp.Exec(ctx, r, t.srcName, t.igName, t.blockNum); err != nil {
    return fmt.Errorf("updating audit status: %w", err)
}

// Trigger Delete/Reindex
// ... continues to delete regardless of retry count
```

**Schema Reference**: The `block_verification` table at [`schema.sql:45`](shovel/schema.sql#L45) has:
```sql
retry_count int not null default 0,
```

But this field is never queried before incrementing.

**Issue**: The code increments `retry_count` but never checks if it exceeds a limit. Blocks that consistently fail audit will retry forever.

**Impact**: MEDIUM - Wasted resources on permanently bad blocks.

**Status**: ✅ Fixed – `Auditor.verify` now reads `retry_count` from `shovel.block_verification`, compares it against `maxAuditRetries` (10), and marks the row as `audit_status = 'failed'` instead of retrying once the limit is reached.

**Recommendation**: Add max retry limit:
```go
// Before retrying, check retry count
const checkRetry = `
    SELECT retry_count FROM shovel.block_verification
    WHERE src_name = $1 AND ig_name = $2 AND block_num = $3
`
var retryCount int
if err := a.pgp.QueryRow(ctx, checkRetry, t.srcName, t.igName, t.blockNum).Scan(&retryCount); err == nil {
    if retryCount >= 10 {
        // Mark as failed permanently
        const fail = `
            UPDATE shovel.block_verification
            SET audit_status = 'failed'
            WHERE src_name = $1 AND ig_name = $2 AND block_num = $3
        `
        _, err := a.pgp.Exec(ctx, fail, t.srcName, t.igName, t.blockNum)
        return err
    }
}
```

---

### 🟡 MEDIUM: Provider Selection Doesn't Avoid Consensus Providers
**File**: [`shovel/audit.go:136-152`](shovel/audit.go#L136-L152)

**Current Code:**
```go
// Pick providers (round-robin based on block num)
providers := a.sources[sc.Name]  // ← Same pool as consensus!
if len(providers) == 0 {
    return fmt.Errorf("no providers for source %s", sc.Name)
}

// Fetch from K providers
k := sc.Audit.ProvidersPerBlock
if k <= 0 {
    k = 1
}
if k > len(providers) {
    k = len(providers)
}

// Simple rotation: start at blockNum % len
startIdx := int(t.blockNum) % len(providers)  // ← Round-robin from same pool
```

**Context**: The providers are populated from the same config URLs at [`audit.go:31-37`](shovel/audit.go#L31-L37):
```go
for _, sc := range conf.Sources {
    var clients []*jrpc2.Client
    for _, u := range sc.URLs {  // ← Same URLs used for consensus
        clients = append(clients, jrpc2.New(u))
    }
    a.sources[sc.Name] = clients
}
```

Compare to consensus providers at [`task.go:896-899`](shovel/task.go#L896-L899):
```go
var providers []*jrpc2.Client
for _, u := range sc.URLs {  // ← Same sc.URLs!
    providers = append(providers, jrpc2.New(u))
}
```

**Issue**: The audit uses the **same provider pool** as consensus. Per the plan (line 54): "replays the exact filter against **two fresh providers** (round-robin)."

The term "fresh" implies providers that **didn't participate in the original consensus**. Using the same providers reduces independence.

**Impact**: MEDIUM - If a provider consistently returns bad data, both consensus and audit may use it.

**Status**: ⚠️ Partially addressed – the code still constructs audit clients from `Source.URLs`, but the intended deployment practice is to configure these URLs such that audit traffic goes to independent infrastructure (e.g., different clusters or vendors) than those used for consensus. Full provider-set separation in code is left as a potential future enhancement.

**Recommendation**: Either:
1. Document that audit providers should be configured separately from consensus
2. Or track which providers were used in consensus and avoid them in audit

---

### 🟢 MINOR: No Metrics for Audit Operations
**File**: [`shovel/audit.go`](shovel/audit.go) - entire file (226 lines, no metrics)

**Current State**: The audit loop logs errors but emits no metrics:
```go
// At audit.go:108-115
if err := a.verify(ctx, sc, t); err != nil {
    slog.ErrorContext(ctx, "verifying block",  // ← Only logging, no metrics
        "src", t.srcName,
        "ig", t.igName,
        "block", t.blockNum,
        "error", err,
    )
}
```

**Comparison**: Phase 1 consensus has metrics at [`shovel/metrics.go:12-33`](shovel/metrics.go#L12-L33):
```go
var (
    ConsensusAttempts = promauto.NewCounterVec(...)
    ConsensusFailures = promauto.NewCounterVec(...)
    ConsensusDuration = promauto.NewHistogramVec(...)
    ProviderErrors = promauto.NewCounterVec(...)
)
```

**Issue**: The audit loop has no Prometheus metrics. The plan specifies several metrics (lines 151-157):
- `shovel_audit_verifications_total`
- `shovel_audit_failures_total`
- `shovel_audit_queue_length`

**Impact**: LOW - Operational visibility issue.

**Status**: ✅ Fixed – `metrics.go` defines `AuditVerifications` and `AuditQueueLength`, and `audit.go` updates these in `check` and `verify`.

**Recommendation**: Add metrics:
```go
var (
    AuditVerifications = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "shovel_audit_verifications_total",
        Help: "Confirmation audits executed",
    }, []string{"src_name", "status"})
    
    AuditQueueLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "shovel_audit_queue_length",
        Help: "Number of confirmed blocks pending audit",
    }, []string{"src_name"})
)

// In check():
AuditQueueLength.WithLabelValues(sc.Name).Set(float64(len(batch)))

// In verify():
if matches == k {
    AuditVerifications.WithLabelValues(t.srcName, "success").Inc()
} else {
    AuditVerifications.WithLabelValues(t.srcName, "failure").Inc()
}
```

---

### 🟢 MINOR: Goroutine Leak on Context Cancel
**File**: [`shovel/audit.go:102-119`](shovel/audit.go#L102-L119)

**Current Code:**
```go
// Process batch
// Limit parallelism
sem := make(chan struct{}, sc.Audit.Parallelism)
var wg sync.WaitGroup
for _, t := range batch {
    wg.Add(1)
    sem <- struct{}{}
    go func(t auditTask) {
        defer wg.Done()
        defer func() { <-sem }()
        if err := a.verify(ctx, sc, t); err != nil {  // ← May block if ctx cancelled
            slog.ErrorContext(ctx, "verifying block",
                "src", t.srcName,
                "ig", t.igName,
                "block", t.blockNum,
                "error", err,
            )
        }
    }(t)
}
wg.Wait()  // ← Hangs if goroutines block
```

**Context**: The `verify` function calls `p.Get()` at [`audit.go:166`](shovel/audit.go#L166) which performs RPC calls:
```go
blocks, err := p.Get(ctx, p.NextURL().String(), &task.filter, t.blockNum, 1)
```

If the underlying HTTP client doesn't properly respect context cancellation, this can hang.

**Issue**: If `ctx` is cancelled while goroutines are running, the `verify` calls may block indefinitely on RPC calls that don't respect the context. The `wg.Wait()` will hang.

**Impact**: LOW - Graceful shutdown issue.

**Status**: ⚠️ Partially addressed – `Auditor.check` now short-circuits if the context is already cancelled before waiting on its worker `WaitGroup`, mirroring the `ctx.Err()` guard used in the consensus engine. Full cancellation of in-flight RPCs still depends on the underlying HTTP client.

**Recommendation**: Add timeout or check context:
```go
select {
case <-ctx.Done():
    return ctx.Err()
default:
    wg.Wait()
}
```

---

## Missing Functionality from Plan

### 📋 Expanded Provider Pool on Failure
**Plan Reference**: Lines 56-57 - "trigger forced reindex with an **expanded provider pool** (e.g., try all configured providers + fallback archival provider)"

**Gap**: When audit fails, it just deletes the block. It doesn't:
1. Try additional providers beyond the configured K
2. Use a fallback archival provider
3. Record which providers failed

**Status**: ✅ Fixed (first stage) – `Auditor.verify` now performs a second pass over **all** configured providers when the initial K-of-K check fails. If every provider that responds matches the stored consensus hash, the block is marked `healthy` and no delete/reindex is triggered. Fallback archival providers remain future work.

**Recommendation**: Implement escalation:
```go
// After normal K providers fail, try all providers
if matches < k {
    for _, p := range providers {
        // try all providers before giving up
    }
}
```

---

### 📋 Block Verification Status Tracking
**Plan Reference**: Line 56 - "writes `shovel.block_verification` rows capturing status"

**Gap**: The audit updates status to 'healthy' or 'retrying', but never creates initial 'pending' status. Phase 1 creates 'healthy' immediately after consensus. This means:
- Blocks go straight from not existing → 'healthy' (Phase 1)
- Audit changes 'healthy' → 'retrying' on failure
- But never 'pending' → 'verifying' → 'healthy'

The state machine from the plan (line 56) isn't fully implemented.

**Status**: ✅ Fixed – Phase 1 now inserts `block_verification` rows with `audit_status = 'pending'`, and Phase 3 updates rows through `verifying` → `healthy` / `retrying` / `failed` based on audit outcomes.

**Recommendation**: Phase 1 should insert with status='pending', audit should update to 'verifying', then 'healthy'.

---

### 📋 Audit Backlog Alert
**Plan Reference**: Lines 130 - "Audit backlog alert: confirmed blocks waiting >30 minutes for audit completion"

**Gap**: No alerting mechanism. Operators can't tell if audit is falling behind.

**Status**: ✅ Fixed – `metrics.go` defines `shovel_audit_backlog_age_seconds`, and `Auditor.check` sets it per source using the oldest `created_at` among the current batch of pending/retrying blocks.

**Recommendation**: Add metric and alert:
```go
// Track oldest pending audit
const oldestQ = `
    SELECT MIN(created_at) FROM shovel.block_verification
    WHERE audit_status IN ('pending', 'retrying')
`
// Expose as metric, alert if age > 30 minutes
```

---

## Positive Findings ✅

1. **Parallelism Control**: Uses semaphore to limit concurrent audits properly.

2. **Automatic Remediation**: Implements delete + reindex on failure (though has race condition).

3. **Round-Robin Provider Selection**: Simple but effective provider rotation strategy.

4. **Query Optimization**: Limits audit batch to 100 blocks to control query cost.

5. **Error Handling**: Individual audit failures don't stop the audit loop.

6. **Status Tracking**: Updates `audit_status` and `retry_count` in database.

7. **Configurable Parameters**: Confirmations, parallelism, and providers per block are configurable.

---

## Testing Gaps

### Missing Tests:
1. **No test** for audit verification matching consensus
2. **No test** for audit mismatch triggering delete
3. **No test** for concurrent audit + task execution (race condition)
4. **No test** for provider round-robin selection
5. **No test** for retry count limiting
6. **No test** for query performance with many integrations
7. **No test** for context cancellation during audit
8. **No test** for batch vs single-block hash comparison

### Recommended Test Cases:
```go
func TestAuditor_VerifyMatch(t *testing.T) {
    // Setup: block_verification with 'pending' status
    // Action: audit runs, providers match consensus hash
    // Verify: status updated to 'healthy'
}

func TestAuditor_VerifyMismatch(t *testing.T) {
    // Setup: block_verification with hash that won't match
    // Action: audit runs
    // Verify: status 'retrying', block deleted, retry_count incremented
}

func TestAuditor_RaceCondition(t *testing.T) {
    // Setup: task actively processing blocks
    // Action: auditor tries to delete block
    // Verify: proper locking prevents corruption
}

func TestAuditor_MaxRetries(t *testing.T) {
    // Setup: block with retry_count = 10
    // Action: audit fails again
    // Verify: status changed to 'failed', not 'retrying'
}
```

---

## Integration with Previous Phases

### Dependencies on Phase 1:
1. **consensus_hash** - Audit compares against this (BROKEN due to batch hashing)
2. **block_verification table** - Audit queries and updates this
3. **HashBlocks** - Audit uses same function (inherits bugs)

### Dependencies on Phase 2:
1. **Status flow** - Audit picks up blocks that Phase 2 marked as 'retrying'
2. None directly, but Phase 2's broken validation means bad status data

### Issues from Previous Phases That Break Phase 3:
1. 🔴 **Phase 1 batch hashing** → Audit can't validate blocks correctly
2. 🔴 **Phase 1 incomplete hash** → Audit produces wrong hashes
3. 🔴 **Phase 2 wrong block validation** → Audit gets inconsistent status
4. 🔴 **Phase 2 no remediation** → Audit must handle all retries

**Critical Path**: Phase 3 **cannot function** until Phase 1's hashing issues are fixed.

---

## Configuration Review

### ✅ Good:
- `Audit` struct has all necessary parameters
- Defaults could be reasonable (need to check unmarshaling)
- `Enabled` flag allows feature toggle

### ⚠️ Issues:
- No defaults set in code (relies on zero values)
- No validation that `ProvidersPerBlock` <= available providers
- No validation that `Confirmations` > 0
- No configuration for tick interval (hardcoded 5s)
- No configuration for max retries
- No configuration for audit queue size (hardcoded 100)

### Recommended Defaults:
```go
if s.Audit.ProvidersPerBlock == 0 {
    s.Audit.ProvidersPerBlock = 2
}
if s.Audit.Confirmations == 0 {
    s.Audit.Confirmations = 128
}
if s.Audit.Parallelism == 0 {
    s.Audit.Parallelism = 4
}
```

---

## Performance Considerations

1. **Database Query Every 5s**: With many sources, this creates constant load
2. **JOIN on task_updates**: Expensive with many integrations
3. **100 Block Batch**: Good limit, but could be configurable
4. **Parallel Verification**: Semaphore properly limits concurrency
5. **No Caching**: Each audit re-fetches from providers (appropriate)

**Recommendations**:
- Add query result caching if same blocks keep appearing
- Consider using database LISTEN/NOTIFY instead of polling
- Profile query performance with realistic data volumes

---

## Security Considerations

1. **Provider Trust**: Audit assumes K honest providers (good assumption with diversification)
2. **Race Condition**: Critical security issue - could corrupt data
3. **No Audit of Auditor**: Who watches the watchmen? No verification that audit itself is correct
4. **Resource Exhaustion**: Unbounded audit queue could exhaust resources

---

## Recommendations Summary

### Must Fix Before Phase 4:
1. 🔴 Fix Phase 1 batch vs per-block hashing (blocks Phase 3 entirely)
2. 🔴 Fix Phase 1 HashBlocks to include log address
3. 🔴 Add locking/coordination to prevent race condition with Task.Delete
4. 🟡 Make audit tick interval configurable
5. 🟡 Add max retry limit to prevent infinite retries

### Should Fix:
1. Add backpressure mechanism for audit queue – ✅ implemented via a global in-flight counter (`inFlight`/`maxInFlight`) in `Auditor`.
2. Optimize database query (avoid JOIN or denormalize) – ✅ implemented with a `MAX(tu.num)` correlated subquery.
3. Add Prometheus metrics for audit operations – ✅ implemented via `AuditVerifications` and `AuditQueueLength`.
4. Enforce provider separation from consensus providers – ⚠️ documented as a deployment/config responsibility; code still uses `Source.URLs`.
5. Add comprehensive integration tests

### Nice to Have:
1. Expanded provider pool on escalation – ✅ first-stage implemented (all providers), archival fallback deferred.
2. Proper status state machine (pending → verifying → healthy) – ✅ implemented via `pending` inserts and `verifying`/`healthy`/`retrying`/`failed` states.
3. Audit backlog alerting – ✅ implemented via `shovel_audit_backlog_age_seconds` and per-source oldest `created_at` tracking.
4. Context cancellation handling – ⚠️ partially implemented with a context short-circuit around `wg.Wait()`; full RPC cancellation depends on HTTP client.
5. Configurable defaults – ⚠️ improved via code defaults for `Audit` and `Consensus` fields; further validation could be added.

---

## Overall Assessment

**Implementation Quality**: 5/10

**Readiness for Phase 4**: ❌ Not Ready

The Phase 3 implementation has **better structure** than Phase 2, with proper separation of concerns and parallelism control. However, it suffers from:

1. **Complete dependency on broken Phase 1 hashing** - Cannot function at all
2. **Critical race condition** - Can corrupt data during concurrent execution
3. **No metrics** - Cannot observe if it's working
4. **Missing functionality** - Provider escalation, proper status flow, alerting

The audit loop will appear to run but will **produce incorrect results** due to the batch hashing mismatch, and may **corrupt data** due to the race condition.

**Recommendation**: 
1. Fix Phase 1 critical issues (#3, #4, #6) first - **BLOCKING**
2. Add locking to prevent Delete race condition - **BLOCKING**
3. Add metrics and testing
4. Then Phase 3 can be functional

Phase 4 (External Repair API) will likely depend on Phase 3 working correctly, as it needs to know which blocks need repair and trigger the same audit/verify/delete flow.

---

## Critical Path Forward

To make the reconciliation strategy functional:

**Phase 1 Fixes (REQUIRED)**:
- ✅ Fix consensus hash to be per-block or properly track batches
- ✅ Include log address in HashBlocks
- ✅ Add reorg detection in consensus path

**Phase 2 Fixes (REQUIRED)**:
- ✅ Validate all blocks in batch, not just last
- ✅ Implement automatic remediation

**Phase 3 Fixes (REQUIRED)**:
- ✅ Add locking to prevent race with tasks
- ✅ Fix batch hash comparison

**Then Phase 3 can actually work and provide the "belt and suspenders" guarantees from the plan.**
