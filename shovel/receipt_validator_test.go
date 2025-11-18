package shovel

import (
	"context"
	"testing"
)

func TestReceiptValidator(t *testing.T) {
	t.Run("Disabled", func(t *testing.T) {
		rv := NewReceiptValidator(nil, false, nil)
		if err := rv.Validate(context.Background(), 1, nil); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
}
