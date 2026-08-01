package state

import (
	"bytes"
	"sort"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

type Reader interface {
	Get(key []byte) ([]byte, bool)
}

type Mutable interface {
	Reader
	Set(key, value []byte)
	Delete(key []byte)
}

type Snapshotter interface {
	Snapshot() []model.StateEntry
}

type Store interface {
	Mutable
	Snapshotter
}

type overlayValue struct {
	key     []byte
	value   []byte
	deleted bool
}

// Overlay isolates transaction writes until the engine decides to commit.
type Overlay struct {
	base   Reader
	writes map[string]overlayValue
}

func NewOverlay(base Reader) *Overlay {
	return &Overlay{
		base:   base,
		writes: make(map[string]overlayValue),
	}
}

func (o *Overlay) Get(key []byte) ([]byte, bool) {
	if pending, ok := o.writes[string(key)]; ok {
		if pending.deleted {
			return nil, false
		}
		return cloneBytes(pending.value), true
	}
	return o.base.Get(key)
}

func (o *Overlay) Set(key, value []byte) {
	o.writes[string(key)] = overlayValue{
		key:   cloneBytes(key),
		value: cloneBytes(value),
	}
}

func (o *Overlay) Delete(key []byte) {
	o.writes[string(key)] = overlayValue{
		key:     cloneBytes(key),
		deleted: true,
	}
}

func (o *Overlay) Changes() []model.StateChange {
	changes := make([]model.StateChange, 0, len(o.writes))
	for _, pending := range o.writes {
		changes = append(changes, model.StateChange{
			Key:    cloneBytes(pending.key),
			Delete: pending.deleted,
			Value:  cloneBytes(pending.value),
		})
	}
	sort.Slice(changes, func(i, j int) bool {
		return bytes.Compare(changes[i].Key, changes[j].Key) < 0
	})
	return changes
}

func (o *Overlay) CommitTo(target Mutable) {
	for _, change := range o.Changes() {
		if change.Delete {
			target.Delete(change.Key)
			continue
		}
		target.Set(change.Key, change.Value)
	}
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
