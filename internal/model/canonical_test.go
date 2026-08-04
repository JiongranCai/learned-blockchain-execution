package model_test

import (
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

func TestCanonicalDigestNormalizesStateAndWrites(t *testing.T) {
	first := model.BlockResult{
		BlockID: "block-1",
		Height:  7,
		Transactions: []model.TxResult{{
			Index:         0,
			TransactionID: "tx-1",
			Status:        model.TxStatusSuccess,
			ReturnValue:   []byte("return"),
			UnitsUsed:     3,
			ComputeDigest: 11,
			Writes: []model.StateChange{
				{Key: []byte("z"), Value: []byte("9")},
				{Key: []byte("a"), Delete: true},
			},
		}},
		FinalState: []model.StateEntry{
			{Key: []byte("z"), Value: []byte("9")},
			{Key: []byte("b"), Value: []byte("2")},
		},
	}
	second := model.BlockResult{
		BlockID: "block-1",
		Height:  7,
		Digest:  "this field is deliberately excluded",
		Transactions: []model.TxResult{{
			Index:         0,
			TransactionID: "tx-1",
			Status:        model.TxStatusSuccess,
			ReturnValue:   []byte("return"),
			UnitsUsed:     3,
			ComputeDigest: 11,
			Writes: []model.StateChange{
				{Key: []byte("a"), Delete: true},
				{Key: []byte("z"), Value: []byte("9")},
			},
		}},
		FinalState: []model.StateEntry{
			{Key: []byte("b"), Value: []byte("2")},
			{Key: []byte("z"), Value: []byte("9")},
		},
	}

	if got, want := model.CanonicalDigest(first), model.CanonicalDigest(second); got != want {
		t.Fatalf("equivalent results produced different digests:\n%s\n%s", got, want)
	}

	second.Transactions[0].ReturnValue = []byte("different")
	if model.CanonicalDigest(first) == model.CanonicalDigest(second) {
		t.Fatal("result-visible return value did not affect digest")
	}
}

func TestCanonicalDigestTotallyOrdersDuplicateKeys(t *testing.T) {
	first := model.BlockResult{
		BlockID: "block-1",
		Transactions: []model.TxResult{{
			TransactionID: "tx-1",
			Status:        model.TxStatusSuccess,
			Writes: []model.StateChange{
				{Key: []byte("same"), Delete: true},
				{Key: []byte("same"), Value: []byte("value")},
			},
		}},
	}
	second := first
	second.Transactions = append([]model.TxResult(nil), first.Transactions...)
	second.Transactions[0].Writes = []model.StateChange{
		{Key: []byte("same"), Value: []byte("value")},
		{Key: []byte("same"), Delete: true},
	}

	if got, want := model.CanonicalDigest(first), model.CanonicalDigest(second); got != want {
		t.Fatalf("duplicate-key tie-break was not canonical:\n%s\n%s", got, want)
	}
}
