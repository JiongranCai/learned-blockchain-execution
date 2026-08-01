package control_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/control"
)

func TestRecorderSnapshotIsConcurrentAndLogicallyStable(t *testing.T) {
	records := []control.EventRecord{
		{Event: control.EventTxEnd, BlockID: "block", TransactionID: "tx-1", TxIndex: 1, Ordinal: 99, Action: "validate"},
		{Event: control.EventBeforeWrite, BlockID: "block", TransactionID: "tx-0", TxIndex: 0, Ordinal: 4, TargetID: "write", Action: "execute"},
		{Event: control.EventEpochStart, BlockID: "block", TargetID: "epoch", Action: "keep"},
		{Event: control.EventTxReady, BlockID: "block", TransactionID: "tx-0", TxIndex: 0, Ordinal: 2, Action: "optimistic"},
		{Event: control.EventBeforeRead, BlockID: "block", TransactionID: "tx-0", TxIndex: 0, Ordinal: 4, TargetID: "read", Action: "execute"},
	}

	var recorder control.Recorder
	var wait sync.WaitGroup
	for index := len(records) - 1; index >= 0; index-- {
		wait.Add(1)
		go func(record control.EventRecord) {
			defer wait.Done()
			recorder.Record(record)
		}(records[index])
	}
	wait.Wait()

	first := recorder.Snapshot()
	second := recorder.Snapshot()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("snapshots are unstable:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if len(first) != len(records) {
		t.Fatalf("lost concurrent records: got %d want %d", len(first), len(records))
	}
	wantEvents := []control.Event{
		control.EventEpochStart,
		control.EventTxReady,
		control.EventBeforeRead,
		control.EventBeforeWrite,
		control.EventTxEnd,
	}
	gotEvents := make([]control.Event, len(first))
	for index := range first {
		gotEvents[index] = first[index].Event
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("unexpected logical order: got %v want %v", gotEvents, wantEvents)
	}

	first[0].Action = "mutated"
	if recorder.Snapshot()[0].Action == "mutated" {
		t.Fatal("Snapshot exposed recorder storage")
	}
}
