package shovel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/indexsupply/shovel/eth"
	"github.com/indexsupply/shovel/jrpc2"
	"github.com/indexsupply/shovel/shovel/glf"
)

var ErrReceiptMismatch = errors.New("receipt mismatch")

type ReceiptValidator struct {
	client  *jrpc2.Client
	enabled bool
	metrics *Metrics
}

func NewReceiptValidator(client *jrpc2.Client, enabled bool, metrics *Metrics) *ReceiptValidator {
	return &ReceiptValidator{
		client:  client,
		enabled: enabled,
		metrics: metrics,
	}
}

func (rv *ReceiptValidator) Enabled() bool {
	return rv.enabled
}

// validateReceipts contains the core hash comparison logic for receipt validation.
// It mirrors the helper-style validate() function in jrpc2/client.go, but is
// specialized for comparing a consensus hash against blocks built from receipts.
func validateReceipts(blocks []eth.Block, consensusHash []byte, metrics *Metrics) error {
	// No blocks returned: treat as an "empty" range and compare against the
	// canonical empty hash. This follows the HashBlocks(nil) semantics used
	// elsewhere (see consensus.go).
	if len(blocks) == 0 {
		emptyHash := HashBlocks(nil)
		if !bytes.Equal(emptyHash, consensusHash) {
			if metrics != nil {
				metrics.ReceiptMismatch()
			}
			return ErrReceiptMismatch
		}
		return nil
	}

	// Non-empty: hash all logs deterministically and compare.
	hash := HashBlocks(blocks)
	if !bytes.Equal(hash, consensusHash) {
		if metrics != nil {
			metrics.ReceiptMismatch()
		}
		return ErrReceiptMismatch
	}
	return nil
}

func (rv *ReceiptValidator) Validate(ctx context.Context, blockNum uint64, consensusHash []byte) error {
	if !rv.enabled {
		return nil
	}

	url := rv.client.NextURL()
	filter := &glf.Filter{
		UseReceipts: true,
	}

	slog.DebugContext(ctx, "receipt-validation-start",
		"block", blockNum,
		"provider", url.String(),
	)

	blocks, err := rv.client.Get(ctx, url.String(), filter, blockNum, 1)
	if err != nil {
		return fmt.Errorf("fetching receipts: %w", err)
	}

	if err := validateReceipts(blocks, consensusHash, rv.metrics); err != nil {
		return err
	}

	empty := len(blocks) == 0
	slog.DebugContext(ctx, "receipt-validation-success",
		"block", blockNum,
		"provider", url.String(),
		"empty", empty,
	)
	return nil
}
