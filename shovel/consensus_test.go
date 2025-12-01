package shovel

import (
	"bytes"
	"context"
	"testing"

	"github.com/indexsupply/shovel/eth"
	"github.com/indexsupply/shovel/jrpc2"
	"github.com/indexsupply/shovel/shovel/config"
	"github.com/indexsupply/shovel/shovel/glf"
)

func TestConsensusEngine_New(t *testing.T) {
	p := []*jrpc2.Client{{}}
	c := config.Consensus{Threshold: 2}
	_, err := NewConsensusEngine(p, c, nil)
	if err == nil {
		t.Error("expected error when threshold > providers")
	}
	c.Threshold = 0
	_, err = NewConsensusEngine(p, c, nil)
	if err == nil {
		t.Error("expected error when threshold < 1")
	}
}

func TestHashBlocks_Deterministic(t *testing.T) {
	// Create two blocks with same logs but different order
	l1 := eth.Log{BlockNumber: eth.Uint64(1), Idx: 1, Data: []byte{1}}
	l2 := eth.Log{BlockNumber: eth.Uint64(1), Idx: 2, Data: []byte{2}}

	b1 := eth.Block{Txs: eth.Txs{{Receipt: eth.Receipt{Logs: eth.Logs{l1, l2}}}}}
	b2 := eth.Block{Txs: eth.Txs{{Receipt: eth.Receipt{Logs: eth.Logs{l2, l1}}}}}

	h1 := HashBlocks([]eth.Block{b1})
	h2 := HashBlocks([]eth.Block{b2})

	if !bytes.Equal(h1, h2) {
		t.Error("hash should be deterministic regardless of log order")
	}
}

// Mock provider that always fails or returns specific blocks
type mockClient struct {
	*jrpc2.Client
	blocks []eth.Block
	err    error
}

func (m *mockClient) Get(ctx context.Context, url string, filter *glf.Filter, start, limit uint64) ([]eth.Block, error) {
	return m.blocks, m.err
}
