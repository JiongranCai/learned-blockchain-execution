package control

import (
	"sort"
	"sync"
)

type EventRecord struct {
	Event         Event  `json:"event"`
	BlockID       string `json:"block_id,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	TxIndex       uint64 `json:"tx_index,omitempty"`
	Incarnation   uint64 `json:"incarnation,omitempty"`
	Ordinal       uint64 `json:"ordinal,omitempty"`
	TargetID      string `json:"target_id,omitempty"`
	Action        string `json:"action"`
}

type Trace struct {
	Engine        string        `json:"engine"`
	PolicyName    string        `json:"policy_name"`
	PolicyVersion string        `json:"policy_version"`
	Events        []EventRecord `json:"events"`
}

// Recorder accepts concurrent event emissions. Snapshot returns a stable
// logical order rather than goroutine completion order.
type Recorder struct {
	mu      sync.Mutex
	records []EventRecord
}

func (r *Recorder) Record(record EventRecord) {
	r.mu.Lock()
	r.records = append(r.records, record)
	r.mu.Unlock()
}

func (r *Recorder) Snapshot() []EventRecord {
	r.mu.Lock()
	records := append([]EventRecord(nil), r.records...)
	r.mu.Unlock()

	sort.SliceStable(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.BlockID != right.BlockID {
			return left.BlockID < right.BlockID
		}
		if left.TransactionID == "" && right.TransactionID != "" {
			return true
		}
		if left.TransactionID != "" && right.TransactionID == "" {
			return false
		}
		if left.TxIndex != right.TxIndex {
			return left.TxIndex < right.TxIndex
		}
		if left.Incarnation != right.Incarnation {
			return left.Incarnation < right.Incarnation
		}
		if left.Ordinal != right.Ordinal {
			return left.Ordinal < right.Ordinal
		}
		if eventRank(left.Event) != eventRank(right.Event) {
			return eventRank(left.Event) < eventRank(right.Event)
		}
		if left.TargetID != right.TargetID {
			return left.TargetID < right.TargetID
		}
		return left.Action < right.Action
	})
	return records
}

func eventRank(event Event) int {
	for index, descriptor := range eventRegistry {
		if descriptor.Event == event {
			return index
		}
	}
	return len(eventRegistry)
}
