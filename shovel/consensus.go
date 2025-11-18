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

func (ce *ConsensusEngine) FetchWithQuorum(ctx context.Context, filter *glf.Filter, start, limit uint64) ([]eth.Block, []byte, error) {
	var (
		conf = ce.config
		t0   = time.Now()
	)
	ce.metrics.Start()
	defer ce.metrics.Stop()

	for attempt := 0; ; attempt++ {
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
				// Use Get method directly, similar to how Task.load works
				// but without the batching loop since we want exact range
				blocks, err := p.Get(ctx, p.NextURL().String(), filter, start, limit)
				if err != nil {
					errs[i] = err
					ce.metrics.ProviderError(p.NextURL().String())
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
			h := HashBlocks(blocks)
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

		// Retry loop continues
	}
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

	// Hash the sorted logs
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
	return eth.Keccak(buf.Bytes())
}
