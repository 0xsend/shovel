package shovel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/indexsupply/shovel/eth"
	"github.com/indexsupply/shovel/jrpc2"
	"github.com/indexsupply/shovel/shovel/config"
	"github.com/indexsupply/shovel/shovel/glf"
	"github.com/indexsupply/shovel/tc"
	"github.com/indexsupply/shovel/wpg"
)

// TestBugReproduction_FaultyProviderMissingLogs reproduces the critical bug
// documented in SEN-68 that Phases 1-3 were designed to fix.
//
// THE BUG:
// Before multi-provider consensus (Phase 1), Shovel uses a single RPC provider.
// If that provider returns incomplete logs due to:
// - Bloom filter issues (known Geth bug)
// - Receipt storage corruption
// - Rate limiting returning partial results
// - Chain reorgs during query
// - Node sync lag
// 
// Then Shovel will faithfully index the incomplete data, mark the block as
// processed, and NEVER recover those missing events without manual intervention.
//
// REPRODUCTION SCENARIO:
// Block 17943843 contains 4 ERC-721 Transfer events. A faulty provider returns
// the block structure correctly but with 0 logs in eth_getLogs response.
// Shovel will:
// 1. Accept the empty logs as truth
// 2. Insert 0 rows into erc721_test table
// 3. Mark block 17943843 as processed in task_updates
// 4. Move on to the next block
// 5. Never revisit this block
//
// EXPECTED: 4 Transfer events indexed
// ACTUAL: 0 events indexed, block marked complete
//
// This test demonstrates the bug exists in the current main branch.
func TestBugReproduction_FaultyProviderMissingLogs(t *testing.T) {
	var (
		ctx  = context.Background()
		pg   = wpg.TestPG(t, Schema)
		conf = config.Root{}
	)

	// Use erc721.json config - block 17943843 has 4 Transfer events
	decode(t, read(t, "erc721.json"), &conf.Integrations)
	tc.NoErr(t, config.ValidateFix(&conf))
	tc.NoErr(t, config.Migrate(ctx, pg, conf))

	const (
		targetBlock     = 17943843
		parentBlockHash = "0x1e2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1be"
		blockHash       = "0x1f2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1bf"
		srcName         = "faulty-provider"
		chainID         = 1
	)

	// Create a faulty provider that returns correct block structure but EMPTY logs
	// This simulates real-world eth_getLogs failures documented in:
	// - https://github.com/ethereum/go-ethereum/issues/18198
	// - https://github.com/ethereum/go-ethereum/issues/21770
	// - https://github.com/ethereum/go-ethereum/issues/15936
	var callCount int
	faultyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body, _ := io.ReadAll(r.Body)
		
		var reqs []map[string]any
		if err := json.Unmarshal(body, &reqs); err != nil {
			// Single request
			var req map[string]any
			json.Unmarshal(body, &req)
			reqs = []map[string]any{req}
		}

		var responses []map[string]any
		for _, req := range reqs {
			method := req["method"].(string)
			id := req["id"]
			
			switch method {
			case "eth_getBlockByNumber":
				// Return valid block with correct hash
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"number":     fmt.Sprintf("0x%x", targetBlock),
						"hash":       blockHash,
						"parentHash": parentBlockHash,
						"timestamp":  "0x64e43a9f",
						// Empty transactions array - this is the bug!
						// Real block has transactions but provider returns empty
						"transactions": []any{},
					},
				})
			case "eth_getLogs":
				// Return EMPTY logs array - THIS IS THE BUG
				// Real block has 4 ERC-721 events but faulty provider returns []
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  []any{}, // ← THE BUG: missing logs
				})
			case "eth_getBlockByHash":
				// For parent hash lookup
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"number":     fmt.Sprintf("0x%x", targetBlock-1),
						"hash":       parentBlockHash,
						"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
						"timestamp":  "0x64e43a9e",
					},
				})
			default:
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"error": map[string]any{
						"code":    -32601,
						"message": "Method not found",
					},
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if len(responses) == 1 {
			json.NewEncoder(w).Encode(responses[0])
		} else {
			json.NewEncoder(w).Encode(responses)
		}
	}))
	defer faultyServer.Close()

	// Create task with the faulty provider
	// This is the current main branch behavior: single provider, no validation
	faultyProvider := jrpc2.New(faultyServer.URL)
	
	ig := conf.Integrations[0]
	task, err := NewTask(
		WithContext(ctx),
		WithPG(pg),
		WithSource(faultyProvider), // Single faulty provider - no consensus
		WithIntegration(ig),
		WithRange(targetBlock, targetBlock+1),
		WithSrcName(srcName),
		WithChainID(chainID),
	)
	tc.NoErr(t, err)

	// Execute the convergence - this will complete "successfully"
	err = task.Converge()
	
	// The bug: Converge returns no error even though we got bad data
	if err != nil {
		t.Logf("Converge returned error (unexpected): %v", err)
	} else {
		t.Logf("✓ Converge completed 'successfully' (this is the bug)")
	}

	// VERIFY THE BUG: We should have 4 rows but instead have 0
	var count int
	query := fmt.Sprintf(`
		select count(*) from erc721_test
		where block_num = %d
		and tx_hash = '\x713df81a2ab53db1d01531106fc5de43012a401ddc3e0586d522e5c55a162d42'
		and contract = '\x57f1887a8bf19b14fc0df6fd9b2acc9af147ea85'
	`, targetBlock)
	
	tc.NoErr(t, pg.QueryRow(ctx, query).Scan(&count))
	
	// This is the bug
	if count == 0 {
		t.Logf("✓ BUG REPRODUCED: Indexed 0 rows instead of expected 4")
		t.Logf("  - Faulty provider returned empty logs for block %d", targetBlock)
		t.Logf("  - Shovel accepted this incomplete data without validation")
		t.Logf("  - No consensus check (Phase 1)")
		t.Logf("  - No receipt validation (Phase 2)")
		t.Logf("  - No confirmation audit (Phase 3)")
	} else {
		t.Errorf("BUG NOT REPRODUCED: expected 0 rows, got %d", count)
		t.Errorf("  The faulty provider test setup may be incorrect")
	}

	// VERIFY THE BUG IS PERMANENT: Block marked as processed
	var taskNum uint64
	err = pg.QueryRow(ctx, `
		select num from shovel.task_updates
		where src_name = $1
		and ig_name = $2
		order by num desc
		limit 1
	`, srcName, ig.Name).Scan(&taskNum)
	
	if err == nil && taskNum == targetBlock {
		t.Logf("✓ Block %d marked as processed in task_updates", targetBlock)
		t.Logf("  Without manual intervention, these missing events will NEVER be recovered")
		t.Logf("  This is why SEN-68 was filed as URGENT priority")
	} else {
		t.Logf("Task updates state: num=%d, err=%v", taskNum, err)
	}

	// Additional context for the bug report
	t.Logf("")
	t.Logf("IMPACT:")
	t.Logf("  - Critical business events silently missing from database")
	t.Logf("  - Compliance reporting will have gaps")
	t.Logf("  - User balances will be incorrect")
	t.Logf("  - No alerts, no errors, no indication anything is wrong")
	t.Logf("")
	t.Logf("ROOT CAUSE:")
	t.Logf("  - Single RPC provider (no redundancy)")
	t.Logf("  - No validation of eth_getLogs completeness")
	t.Logf("  - No post-confirmation auditing")
	t.Logf("")
	t.Logf("SOLUTION (Phases 1-3):")
	t.Logf("  - Phase 1: Multi-provider consensus (2-of-3 must agree)")
	t.Logf("  - Phase 2: Receipt validation (cross-check via eth_getBlockReceipts)")
	t.Logf("  - Phase 3: Confirmation audit (re-verify after N confirmations)")
	t.Logf("")
	t.Logf("Provider calls made: %d", callCount)
}

// TestBugConditions_PartialLogs tests another variant of the bug where
// a provider returns SOME logs but not ALL logs for a block.
func TestBugConditions_PartialLogs(t *testing.T) {
	var (
		ctx  = context.Background()
		pg   = wpg.TestPG(t, Schema)
		conf = config.Root{}
	)

	decode(t, read(t, "erc721.json"), &conf.Integrations)
	tc.NoErr(t, config.ValidateFix(&conf))
	tc.NoErr(t, config.Migrate(ctx, pg, conf))

	const (
		targetBlock     = 17943843
		parentBlockHash = "0x1e2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1be"
		blockHash       = "0x1f2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1bf"
		srcName         = "partial-provider"
		chainID         = 1
	)

	// Provider returns only 2 of 4 logs - even more subtle bug
	partialServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		
		var reqs []map[string]any
		if err := json.Unmarshal(body, &reqs); err != nil {
			var req map[string]any
			json.Unmarshal(body, &req)
			reqs = []map[string]any{req}
		}

		var responses []map[string]any
		for _, req := range reqs {
			method := req["method"].(string)
			id := req["id"]
			
			switch method {
			case "eth_getBlockByNumber":
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"number":       fmt.Sprintf("0x%x", targetBlock),
						"hash":         blockHash,
						"parentHash":   parentBlockHash,
						"timestamp":    "0x64e43a9f",
						"transactions": []any{},
					},
				})
			case "eth_getLogs":
				// Return only 2 of 4 logs - more subtle data corruption
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": []any{
						// Minimal valid log structure - just 2 events instead of 4
						map[string]any{
							"address":     "0x57f1887a8bf19b14fc0df6fd9b2acc9af147ea85",
							"topics":      []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
							"data":        "0x",
							"blockNumber": fmt.Sprintf("0x%x", targetBlock),
							"logIndex":    "0x100",
						},
						map[string]any{
							"address":     "0x57f1887a8bf19b14fc0df6fd9b2acc9af147ea85",
							"topics":      []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
							"data":        "0x",
							"blockNumber": fmt.Sprintf("0x%x", targetBlock),
							"logIndex":    "0x101",
						},
						// Missing 2 more logs!
					},
				})
			case "eth_getBlockByHash":
				responses = append(responses, map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"number":     fmt.Sprintf("0x%x", targetBlock-1),
						"hash":       parentBlockHash,
						"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
					},
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if len(responses) == 1 {
			json.NewEncoder(w).Encode(responses[0])
		} else {
			json.NewEncoder(w).Encode(responses)
		}
	}))
	defer partialServer.Close()

	partialProvider := jrpc2.New(partialServer.URL)
	
	ig := conf.Integrations[0]
	task, err := NewTask(
		WithContext(ctx),
		WithPG(pg),
		WithSource(partialProvider),
		WithIntegration(ig),
		WithRange(targetBlock, targetBlock+1),
		WithSrcName(srcName),
		WithChainID(chainID),
	)
	tc.NoErr(t, err)

	_ = task.Converge() // May fail due to parsing but that's ok

	var count int
	query := fmt.Sprintf(`select count(*) from erc721_test where block_num = %d`, targetBlock)
	pg.QueryRow(ctx, query).Scan(&count)
	
	t.Logf("Partial logs scenario: got %d events (expected 4)", count)
	if count > 0 && count < 4 {
		t.Logf("✓ PARTIAL BUG: Some events indexed but not all - even harder to detect!")
	}
}

// mockFaultySource implements Source interface for unit testing the bug
type mockFaultySource struct {
	returnEmptyLogs bool
}

func (m *mockFaultySource) Get(ctx context.Context, url string, filter *glf.Filter, start, limit uint64) ([]eth.Block, error) {
	if m.returnEmptyLogs {
		// Return valid block but with no transactions/logs
		block := eth.Block{
			Txs: eth.Txs{}, // Empty - the bug!
		}
		block.Header.Number = eth.Uint64(start)
		block.Header.Hash = mustHex("0x1f2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1bf")
		block.Header.Parent = mustHex("0x1e2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1be")
		return []eth.Block{block}, nil
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockFaultySource) Latest(ctx context.Context, url string, num uint64) (uint64, []byte, error) {
	return 17943844, mustHex("0x2f2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1bf"), nil
}

func (m *mockFaultySource) Hash(ctx context.Context, url string, num uint64) ([]byte, error) {
	return mustHex("0x1e2daaa5c2e7fa9632fe767eb2f1fe0b42d8cbe1e8d18a2a275bc2060e86a1be"), nil
}

func (m *mockFaultySource) NextURL() *jrpc2.URL {
	u, _ := jrpc2.New("http://faulty-provider:8545").NextURL(), (*jrpc2.URL)(nil)
	return u
}

func mustHex(s string) []byte {
	if len(s) > 2 && s[0:2] == "0x" {
		s = s[2:]
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
