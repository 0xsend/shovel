package shovel

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/indexsupply/shovel/eth"
	"github.com/indexsupply/shovel/jrpc2"
	"github.com/indexsupply/shovel/shovel/config"
	"github.com/indexsupply/shovel/shovel/glf"
	"golang.org/x/sync/errgroup"
)

type ConsensusEngine struct {
	providers []*jrpc2.Client
	config    config.Consensus
	metrics   *Metrics
}

func NewConsensusEngine(providers []*jrpc2.Client, conf config.Consensus, metrics *Metrics) (*ConsensusEngine, error) {
	if conf.Threshold > len(providers) {
		return nil, fmt.Errorf("threshold (%d) cannot exceed providers (%d)", conf.Threshold, len(providers))
	}
	if conf.Threshold < 1 {
		return nil, fmt.Errorf("threshold must be >= 1")
	}
	return &ConsensusEngine{
		providers: providers,
		config:    conf,
		metrics:   metrics,
	}, nil
}

const maxConsensusAttempts = 1000

func (ce *ConsensusEngine) FetchWithQuorum(ctx context.Context, filter *glf.Filter, start, limit uint64) ([]eth.Block, []byte, error) {
	var (
		conf = ce.config
		t0   = time.Now()
	)
	ce.metrics.Start()
	defer ce.metrics.Stop()

	for attempt := 0; attempt < maxConsensusAttempts; attempt++ {
		// Short-circuit if context is already cancelled
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

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

		// Parallel fetch
		var (
			eg        errgroup.Group
			responses = make([][]eth.Block, len(ce.providers))
			errs      = make([]error, len(ce.providers))
		)
		for i, p := range ce.providers {
			i, p := i, p
			eg.Go(func() error {
				// Cache URL at start to avoid rotation drift across multiple calls
				url := p.NextURL()
				// Use Get method directly, similar to how Task.load works
				// but without the batching loop since we want exact range
				blocks, err := p.Get(ctx, url.String(), filter, start, limit)
				if err != nil {
					errs[i] = err
					ce.metrics.ProviderError(url.String())
					return nil // Don't fail the group, we handle errors individually
				}
				responses[i] = blocks
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, nil, err // Should not happen given we return nil above
		}

		// Compute hashes and count votes
		var (
			counts    = make(map[string]int)
			canonHash string
			canonIdx  int = -1
		)
		for i, blocks := range responses {
			if errs[i] != nil {
				continue
			}
			h := HashBlocksWithRange(blocks, start, limit)
			s := string(h)
			counts[s]++
			if counts[s] >= conf.Threshold {
				canonHash = s
				canonIdx = i
				break
			}
		}

		if canonIdx >= 0 {
			slog.DebugContext(ctx, "consensus-reached",
				"n", start,
				"threshold", conf.Threshold,
				"total", len(ce.providers),
				"attempt", attempt+1,
				"elapsed", time.Since(t0),
			)
			return responses[canonIdx], []byte(canonHash), nil
		}

		ce.metrics.Failure()
		slog.WarnContext(ctx, "consensus-failed",
			"n", start,
			"threshold", conf.Threshold,
			"votes", fmt.Sprintf("%v", counts),
			"attempt", attempt+1,
		)
	}

	return nil, nil, fmt.Errorf("consensus not reached after %d attempts", maxConsensusAttempts)
}

// HashBlocksWithRange computes a deterministic hash of the logs in the blocks,
// and for empty ranges includes the requested block range so distinct empty
// ranges do not collide.
func HashBlocksWithRange(blocks []eth.Block, start, limit uint64) []byte {
	if len(blocks) == 0 {
		return eth.Keccak([]byte(fmt.Sprintf("empty-range-%d-%d", start, limit)))
	}
	return HashBlocks(blocks)
}

// HashBlocks computes a deterministic hash of the logs in the blocks.
// It reuses the existing eth.Keccak/Hash logic.
// Logs are sorted by blockNum, txHash, logIdx before hashing to ensure
// determinism regardless of provider return order (though Get usually returns sorted).
func HashBlocks(blocks []eth.Block) []byte {
	if len(blocks) == 0 {
		return eth.Keccak([]byte("empty"))
	}

	// Collect all logs
	var logs []eth.Log
	for _, b := range blocks {
		for _, tx := range b.Txs {
			logs = append(logs, tx.Logs...)
		}
	}

	// Sort logs deterministically
	slices.SortFunc(logs, func(a, b eth.Log) int {
		if uint64(a.BlockNumber) < uint64(b.BlockNumber) {
			return -1
		}
		if uint64(a.BlockNumber) > uint64(b.BlockNumber) {
			return 1
		}
		if n := bytes.Compare(a.TxHash, b.TxHash); n != 0 {
			return n
		}
		return int(a.Idx) - int(b.Idx)
	})

	// Hash the sorted logs, including address to avoid collisions
	var buf bytes.Buffer
	for _, l := range logs {
		buf.Write(l.Address.Bytes())
		buf.Write([]byte(eth.EncodeUint64(uint64(l.BlockNumber))))
		buf.Write(l.TxHash.Bytes())
		buf.Write([]byte(eth.EncodeUint64(uint64(l.Idx))))
		buf.Write([]byte(eth.EncodeUint64(uint64(l.TxIdx)))) // transaction index
		buf.Write(l.Data.Bytes())
		for _, t := range l.Topics {
			buf.Write(t.Bytes())
		}
	}
	return eth.Keccak(buf.Bytes())
}
