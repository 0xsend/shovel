package shovel

import (
	"bytes"
	"context"
	"errors"
	"fmt"

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
	hash := HashBlocks(blocks)
	if !bytes.Equal(hash, consensusHash) {
		rv.metrics.ReceiptMismatch()
		return ErrReceiptMismatch
	}
	return nil
}
