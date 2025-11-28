# Phase 2 Review: Receipt-Level Validation

## Overview
This review covers the implementation of Phase 2 from `shovel_changes.md`: Receipt Validation. The goal is to fetch receipts from a secondary provider after consensus and verify log data matches.

## Summary of Changes
- Added `ReceiptValidator` to validate blocks using receipt data
- Added `ReceiptVerifier` configuration to sources
- Integrated receipt validation into task convergence flow
- Added Prometheus metric for receipt mismatches
- Minimal test coverage (disabled case only)

## Critical Issues Found

### 🔴 CRITICAL: Validates Wrong Block Number ✅ Implemented
**Files**: [`shovel/receipt_validator.go:33-46`](shovel/receipt_validator.go#L33-L46) and [`shovel/task.go:468-481`](shovel/task.go#L468-L481)

**Receipt Validator Code:**
```go
func (rv *ReceiptValidator) Validate(ctx context.Context, blockNum uint64, consensusHash []byte) error {
    if !rv.enabled {
        return nil
    }
    filter := &glf.Filter{
        UseReceipts: true,
    }
    blocks, err := rv.client.Get(ctx, rv.client.NextURL().String(), filter, blockNum, 1)
    if err != nil {
        return fmt.Errorf("fetching receipts: %w", err)
    }
    if len(blocks) == 0 {
        return fmt.Errorf("no blocks returned for receipt validation")
    }
    
    // HashBlocks sorts logs deterministically, same as consensus engine
    hash := HashBlocks(blocks)  // ← Hashes single block
    if !bytes.Equal(hash, consensusHash) {
        rv.metrics.ReceiptMismatch()
        return ErrReceiptMismatch
    }
    return nil
}
```

**Task Converge Code:**
```go
if task.receiptValidator != nil && task.receiptValidator.Enabled() {
    var cHash []byte
    if len(consensusHash) > 0 {
        cHash = consensusHash  // ← Hash from entire batch (computed at line 437)
    } else {
        cHash = HashBlocks(blocks)
    }
    last := blocks[len(blocks)-1]  // ← Only last block
    err := task.receiptValidator.Validate(ctx, last.Num(), cHash)  // ← Validates only last block!
    if err != nil {
        pgtx.Rollback(ctx)
        slog.ErrorContext(ctx, "receipt-mismatch", "block", last.Num(), "error", err)
        return fmt.Errorf("validating receipts: %w", err)
    }
}
```

**Context**: The `consensusHash` was computed for ALL blocks in the batch at [`task.go:437`](shovel/task.go#L437):
```go
blocks, consensusHash, err = task.consensus.FetchWithQuorum(ctx, &task.filter, localNum+1, delta)
```

Where `delta` can be up to `task.batchSize` blocks (e.g., 10-100 blocks).

**Issue**: The code validates only the **last block** in the batch, but the `consensusHash` was computed across **all blocks** in the batch (Phase 1 issue). This means:
1. If batch is blocks 100-110, only block 110 is validated
2. Blocks 100-109 are never verified via receipts
3. The hash won't match because it's comparing single-block hash vs batch hash

**Impact**: CRITICAL - Receipt validation is effectively broken. Most blocks skip validation entirely.

**Recommendation**: Either validate each block individually or fix the consensus hash storage strategy from Phase 1:
```go
// Option 1: Validate each block in the batch
for _, b := range blocks {
    blockHash := HashBlocks([]eth.Block{b})
    if err := task.receiptValidator.Validate(ctx, b.Num(), blockHash); err != nil {
        // handle error
    }
}

// Option 2: Store batch range in validation (requires Phase 1 fix)
// Validate the entire batch at once
```

---

### 🔴 CRITICAL: No Reorg Handling After Mismatch ✅ Implemented
**File**: [`shovel/task.go:476-481`](shovel/task.go#L476-L481)

**Current Code:**
```go
err := task.receiptValidator.Validate(ctx, last.Num(), cHash)
if err != nil {
    pgtx.Rollback(ctx)
    slog.ErrorContext(ctx, "receipt-mismatch", "block", last.Num(), "error", err)
    return fmt.Errorf("validating receipts: %w", err)  // ← Just returns, no cleanup!
}
```

**Comparison with Reorg Handling**: At [`task.go:442-450`](shovel/task.go#L442-L450), when a reorg is detected, the code properly deletes the block:
```go
if errors.Is(err, ErrReorg) {
    slog.ErrorContext(ctx, "reorg",
        "n", localNum,
        "h", fmt.Sprintf("%.4x", localHash),
    )
    if err := task.Delete(pgtx, localNum); err != nil {
        return fmt.Errorf("deleting during reorg: %w", err)
    }
    continue  // ← Retries automatically
}
```

**Issue**: Per the plan (lines 46-49), when receipt verification fails, the system should:
1. Remove the block via existing reorg cleanup
2. Automatically re-fetch using a fresh provider subset

**Current behavior**: Just returns an error and stops the task. The block remains in a bad state with no automatic remediation.

**Impact**: CRITICAL - Defeats the purpose of receipt validation. Bad data persists.

**Recommendation**: Implement automatic remediation as specified in the plan:
```go
if err != nil {
    pgtx.Rollback(ctx)
    slog.ErrorContext(ctx, "receipt-mismatch", "block", last.Num(), "error", err)
    
    // Per plan: Remove the block via existing reorg cleanup
    if err := task.Delete(pgtx, last.Num()); err != nil {
        return fmt.Errorf("deleting mismatched block: %w", err)
    }
    
    // Update block_verification status to 'retrying'
    const q = `update shovel.block_verification 
               set audit_status = 'retrying', retry_count = retry_count + 1
               where src_name = $1 and ig_name = $2 and block_num = $3`
    _, err := pgtx.Exec(ctx, q, task.srcName, task.destConfig.Name, last.Num())
    if err != nil {
        return fmt.Errorf("updating verification status: %w", err)
    }
    
    // Return ErrReceiptMismatch to trigger retry on next iteration
    return ErrReceiptMismatch
}
```

---

### 🟡 HIGH: Not Using Different Provider Class ✅ Implemented
**File**: [`shovel/task.go:895-902`](shovel/task.go#L895-L902)

**Current Code:**
```go
var rv *ReceiptValidator
if sc.ReceiptVerifier.Enabled {
    url := sc.ReceiptVerifier.Provider
    if url == "" && len(sc.URLs) > 0 {
        url = sc.URLs[len(sc.URLs)-1]  // ← Uses last consensus provider!
    }
    rv = NewReceiptValidator(jrpc2.New(url), true, NewMetrics(sc.Name))
}
```

**Context**: The `sc.URLs` are the same URLs used for consensus at [`task.go:887-888`](shovel/task.go#L887-L888):
```go
for _, u := range sc.URLs {
    providers = append(providers, jrpc2.New(u))
}
```

**Configuration Structure**: Per [`config/config.go:355-358`](shovel/config/config.go#L355-L358):
```go
type ReceiptVerifier struct {
    Provider string `json:"provider"`  // Optional
    Enabled  bool   `json:"enabled"`
}
```

No validation that `Provider` differs from consensus URLs.

**Issue**: The plan explicitly states (line 45): "fetch `eth_getBlockReceipts` or `eth_getTransactionReceipt` for the same block from a **different provider class** (e.g., Erigon + Geth mix)."

**Current implementation**: If no provider is specified, it defaults to the **last URL from the same provider list** used for consensus. This doesn't provide independent verification.

**Impact**: MEDIUM-HIGH - Receipt verification uses the same provider that just participated in consensus, reducing its effectiveness at catching provider-specific issues.

**Recommendation**: Enforce different provider requirement:
```go
if sc.ReceiptVerifier.Enabled {
    if sc.ReceiptVerifier.Provider == "" {
        return nil, fmt.Errorf("receipt_verifier requires explicit provider URL (must differ from consensus providers)")
    }
    rv = NewReceiptValidator(jrpc2.New(sc.ReceiptVerifier.Provider), true, NewMetrics(sc.Name))
}
```

Or at minimum, document that the receipt provider should be architecturally different (different node implementation, infrastructure, etc.).

---

### 🟡 HIGH: Missing Transaction-Level Receipt Validation
**File**: [`shovel/receipt_validator.go:33-55`](shovel/receipt_validator.go#L33-L55)

**Current Code:**
```go
func (rv *ReceiptValidator) Validate(ctx context.Context, blockNum uint64, consensusHash []byte) error {
    if !rv.enabled {
        return nil
    }
    filter := &glf.Filter{
        UseReceipts: true,  // ← Only uses eth_getBlockReceipts
    }
    blocks, err := rv.client.Get(ctx, rv.client.NextURL().String(), filter, blockNum, 1)
    if err != nil {
        return fmt.Errorf("fetching receipts: %w", err)
    }
    if len(blocks) == 0 {
        return fmt.Errorf("no blocks returned for receipt validation")
    }

    // HashBlocks sorts logs deterministically, same as consensus engine
    hash := HashBlocks(blocks)  // ← Hashes entire block structure
    if !bytes.Equal(hash, consensusHash) {
        rv.metrics.ReceiptMismatch()
        return ErrReceiptMismatch
    }
    return nil
}
```

**Issue**: The plan mentions "fetch `eth_getBlockReceipts` **or `eth_getTransactionReceipt`**" (line 45). The current implementation only uses `eth_getBlockReceipts` via the `UseReceipts: true` filter flag. The plan also states (line 48): "Receipts are reduced to their **log hashes** and compared to inserted data."

**Current behavior**: Hashes entire blocks including transaction metadata. Doesn't support per-transaction validation.

**Impact**: MEDIUM - Less flexibility in verification strategy. Can't detect transaction-level inconsistencies.

**Recommendation**: Add support for transaction-level receipt verification:
```go
// Option to validate per-transaction instead of per-block
func (rv *ReceiptValidator) ValidateTransactions(ctx context.Context, txHashes [][]byte, consensusHash []byte) error {
    // Fetch individual tx receipts via eth_getTransactionReceipt
    // Extract logs and compare
}
```

---

### 🟡 MEDIUM: Hash Comparison Inherits Phase 1 Bugs ✅ Addressed in Phase 1 HashBlocks
**File**: [`shovel/receipt_validator.go:49-52`](shovel/receipt_validator.go#L49-L52)

**Current Code:**
```go
// HashBlocks sorts logs deterministically, same as consensus engine
hash := HashBlocks(blocks)  // ← Uses Phase 1's HashBlocks function
if !bytes.Equal(hash, consensusHash) {
    rv.metrics.ReceiptMismatch()
    return ErrReceiptMismatch
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

**Issue**: This uses `HashBlocks` from Phase 1, which has the bug where log addresses aren't included in the hash (Phase 1 review issue #3). This means receipt validation would miss address mismatches.

**Impact**: MEDIUM - Receipt validation could produce false positives (thinking blocks match when they don't).

**Recommendation**: Must fix `HashBlocks` in Phase 1 first. Include log address:
```go
// In consensus.go HashBlocks function
buf.Write(l.Address)  // ADD THIS
buf.Write(l.TxHash)
buf.Write([]byte(eth.EncodeUint64(uint64(l.BlockNumber))))
// ... rest of fields
```

---

### 🟡 MEDIUM: No Retry Limit for Receipt Mismatches ✅ Implemented
**File**: [`shovel/task.go:476-481`](shovel/task.go#L476-L481)

**Current Code:**
```go
err := task.receiptValidator.Validate(ctx, last.Num(), cHash)
if err != nil {
    pgtx.Rollback(ctx)
    slog.ErrorContext(ctx, "receipt-mismatch", "block", last.Num(), "error", err)
    return fmt.Errorf("validating receipts: %w", err)  // ← No retry count check
}
```

**Schema Reference**: The `block_verification` table at [`shovel/schema.sql:45`](shovel/schema.sql#L45) has:
```sql
retry_count int not null default 0,
```

But the code never queries or checks this field before validation.

**Issue**: The plan specifies using `retry_count` in the `block_verification` table to track retry attempts, but the code doesn't check or enforce a maximum retry count for receipt validation failures.

**Impact**: MEDIUM - Could cause infinite retry loops if receipts consistently disagree.

**Recommendation**: Add max retry check:
```go
// Before validation, check retry count
const checkRetryQ = `
    select retry_count from shovel.block_verification
    where src_name = $1 and ig_name = $2 and block_num = $3
`
var retryCount int
err := pgtx.QueryRow(ctx, checkRetryQ, task.srcName, task.destConfig.Name, last.Num()).Scan(&retryCount)
if err == nil && retryCount > 5 {
    return fmt.Errorf("max receipt validation retries exceeded for block %d", last.Num())
}
```

---

### 🟢 MINOR: Missing Context in Validation ✅ Implemented
**File**: [`shovel/receipt_validator.go:33-55`](shovel/receipt_validator.go#L33-L55)

**Current Code:**
```go
func (rv *ReceiptValidator) Validate(ctx context.Context, blockNum uint64, consensusHash []byte) error {
    if !rv.enabled {
        return nil
    }
    filter := &glf.Filter{
        UseReceipts: true,
    }
    blocks, err := rv.client.Get(ctx, rv.client.NextURL().String(), filter, blockNum, 1)
    // ← No logging of provider URL or validation attempt
    if err != nil {
        return fmt.Errorf("fetching receipts: %w", err)
    }
    // ... rest of validation
}
```

**Issue**: The validation function doesn't log which provider is being used for receipt verification, making debugging difficult. When validation fails, you can't tell which provider was consulted.

**Impact**: LOW - Operational visibility issue.

**Recommendation**: Add logging:
```go
func (rv *ReceiptValidator) Validate(ctx context.Context, blockNum uint64, consensusHash []byte) error {
    if !rv.enabled {
        return nil
    }
    
    provider := rv.client.NextURL().String()
    slog.DebugContext(ctx, "receipt-validation-start", 
        "block", blockNum, 
        "provider", provider)
    
    // ... existing logic
    
    slog.DebugContext(ctx, "receipt-validation-success", 
        "block", blockNum, 
        "provider", provider)
    return nil
}
```

---

### 🟢 MINOR: No Handling of Empty Blocks ✅ Implemented
**File**: [`shovel/receipt_validator.go:44-46`](shovel/receipt_validator.go#L44-L46)

**Current Code:**
```go
blocks, err := rv.client.Get(ctx, rv.client.NextURL().String(), filter, blockNum, 1)
if err != nil {
    return fmt.Errorf("fetching receipts: %w", err)
}
if len(blocks) == 0 {
    return fmt.Errorf("no blocks returned for receipt validation")  // ← Treats empty as error
}

// HashBlocks sorts logs deterministically, same as consensus engine
hash := HashBlocks(blocks)
```

**Context**: The `HashBlocks` function at [`shovel/consensus.go:132-135`](shovel/consensus.go#L132-L135) handles empty blocks:
```go
func HashBlocks(blocks []eth.Block) []byte {
    if len(blocks) == 0 {
        return eth.Keccak([]byte("empty"))
    }
    // ...
}
```

**Issue**: If a block has no transactions, `eth_getBlockReceipts` might return an empty array (valid) vs an error. The code treats this as an error instead of passing it to `HashBlocks` for proper empty block handling.

**Impact**: LOW - Could cause false failures for empty blocks.

**Recommendation**: Handle empty blocks gracefully:
```go
if len(blocks) == 0 {
    // Empty block is valid - hash should match empty consensus
    emptyHash := HashBlocks([]eth.Block{})
    if !bytes.Equal(emptyHash, consensusHash) {
        return ErrReceiptMismatch
    }
    return nil
}
```

---

## Missing Functionality from Plan

### 📋 Secondary Provider Configuration ✅ Implemented
**Plan Reference**: Line 45 - "different provider class"

**Gap**: No validation or enforcement that the receipt verifier provider is actually different from consensus providers. Config accepts any URL without checking.

**Recommendation**: Add validation in `config.go`:
```go
// In Source.UnmarshalJSON
if s.ReceiptVerifier.Enabled {
    if s.ReceiptVerifier.Provider == "" {
        return fmt.Errorf("receipt_verifier.provider must be specified when enabled")
    }
    // Check it's not in consensus URLs
    for _, u := range s.URLs {
        if u == s.ReceiptVerifier.Provider {
            return fmt.Errorf("receipt_verifier.provider must differ from consensus providers")
        }
    }
}
```

---

### 📋 Block Deletion on Mismatch ✅ Implemented
**Plan Reference**: Lines 47-48 - "Removal of the block via existing reorg cleanup (delete rows ≥ block)"

**Gap**: Missing implementation. Receipt mismatch just returns error without cleanup.

**Recommendation**: See Critical Issue #2 above.

---

### 📋 Verification Status Updates ✅ Implemented
**Plan Reference**: Line 48 - "set verification status to `retrying`"

**Gap**: The `block_verification.audit_status` field is never updated to 'retrying' when receipt validation fails.

**Recommendation**: Update status as part of remediation (see Critical Issue #2).

---

## Positive Findings ✅

1. **Simple Design**: The `ReceiptValidator` is cleanly separated from consensus logic.

2. **Metric Added**: `shovel_receipt_mismatch_total` metric properly tracks failures.

3. **Transaction Safety**: Receipt validation runs within the same transaction as data inserts, preventing partial writes.

4. **Reuses Existing Code**: Leverages `HashBlocks` and `client.Get` for consistency.

5. **Optional Feature**: Can be disabled via config, allowing gradual rollout.

6. **Error Propagation**: Receipt mismatches properly fail the task convergence.

---

## Testing Gaps

### Missing Tests:
1. ✅ Test added for receipt validation matching consensus (see TestValidateReceipts_Match)
2. ✅ Test added for receipt mismatch detection (see TestValidateReceipts_Mismatch / _EmptyMismatch)
3. **No test** for automatic remediation after mismatch
4. **No dedicated test** for batch vs single-block validation (covered indirectly by per-block validation logic)
5. ✅ Test added for empty/"no blocks" handling (see TestValidateReceipts_EmptyMatch / _EmptyMismatch)
6. **No test** for different provider usage
7. **No test** for metrics emission

### Recommended Test Cases:
```go
func TestReceiptValidator_Match(t *testing.T) {
    // Setup: consensus hash from mock provider
    // Action: validate with matching receipts
    // Verify: no error, no metrics
}

func TestReceiptValidator_Mismatch(t *testing.T) {
    // Setup: consensus hash that doesn't match receipts
    // Action: validate
    // Verify: ErrReceiptMismatch, metric incremented
}

func TestTask_Converge_ReceiptMismatchRemediation(t *testing.T) {
    // Setup: receipt validator that fails
    // Action: converge
    // Verify: block deleted, status set to 'retrying'
}

func TestReceiptValidator_EmptyBlock(t *testing.T) {
    // Setup: block with no transactions
    // Action: validate
    // Verify: handles gracefully
}
```

---

## Configuration Review

### ✅ Good:
- `ReceiptVerifier` struct is simple and clear
- Provider URL can be explicitly configured
- Enabled flag allows feature toggle

### ⚠️ Issues:
- No validation that provider differs from consensus providers
- No validation that provider URL is valid/reachable
- Defaults to last consensus provider if not specified (bad for independence)
- No configuration for validation strategy (per-block vs per-batch)

---

## Integration with Phase 1

### Dependencies on Phase 1:
1. **consensus Hash**: Receipt validation compares against consensus hash from Phase 1
2. **HashBlocks**: Reuses the same hashing function (inherits bugs)
3. **block_verification table**: Writes to same table as consensus engine

### Issues from Phase 1 That Break Phase 2:
1. ✅ Missing provider set tracking → Not directly blocking, but reduces debuggability
2. 🔴 **No reorg detection** → If Phase 1 misses reorg, Phase 2 won't help
3. 🔴 **Incomplete log hashing** → Receipt validation will produce wrong hashes
4. 🔴 **Batch vs per-block hash mismatch** → Receipt validation validates wrong block(s)
5. ✅ Config validation → Affects both phases

**Recommendation**: Phase 1 issues #2, #3, #4 must be fixed before Phase 2 can function correctly.

---

## Performance Considerations

1. **Additional RPC Call**: Each batch now makes an extra `eth_getBlockReceipts` call, increasing RPC cost
2. **Validation Blocking**: Receipt validation runs synchronously in the convergence path, adding latency
3. **No Caching**: Receipts aren't cached, so retries always re-fetch

**Recommendations**:
- Consider making receipt validation async (validate in background, mark status in DB)
- Add receipt caching if retry rates are high
- Provide tuning knob for validation frequency (e.g., validate every Nth block)

---

## Security Considerations

1. **Provider Trust**: Receipt validation assumes the receipt provider is trustworthy. If compromised, it's ineffective.
2. **Hash Collision**: Inherits same collision resistance as Phase 1 (Keccak256) ✅
3. **No Timeout**: Receipt validation has no explicit timeout, could hang indefinitely ⚠️

---

## Recommendations Summary

### Must Fix Before Phase 3:
1. 🔴 Fix validation to handle all blocks in batch, not just last block
2. 🔴 Implement automatic block deletion and remediation on mismatch
3. 🟡 Enforce different provider class requirement
4. 🟡 Fix Phase 1 hash computation to include log addresses

### Should Fix:
1. Add retry limit for receipt mismatches
2. Add transaction-level receipt validation support
3. Improve provider logging for debugging
4. Add config validation for provider uniqueness
5. Add comprehensive integration tests

### Nice to Have:
1. Empty block handling
2. Async validation
3. Receipt caching
4. Validation frequency tuning

---

## Overall Assessment

**Implementation Quality**: 4/10

**Readiness for Phase 3**: ❌ Not Ready

The Phase 2 implementation has the right structure but **critical functionality is missing or broken**:

1. Only validates last block instead of entire batch
2. No automatic remediation when mismatches are detected
3. Doesn't enforce using different provider class
4. Inherits blocking bugs from Phase 1 (hash computation, batch strategy)

The implementation currently provides a **false sense of security** - it looks like receipt validation is happening, but it's not actually validating most blocks and doesn't auto-remediate when failures occur.

**Recommendation**: 
1. Fix Phase 1 critical issues (#2, #3, #4) first
2. Then address the 4 "Must Fix" items above
3. Add comprehensive testing before proceeding to Phase 3

Phase 3 (Audit Loop) depends on accurate block verification records, which Phase 2 is supposed to create. Without working receipt validation, the audit loop will have nothing reliable to check.
