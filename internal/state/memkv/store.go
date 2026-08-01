package memkv

import (
	"bytes"
	"errors"
	"sort"
	"sync"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
)

var ErrDuplicateKey = errors.New("duplicate state key")

type Store struct {
	mu   sync.RWMutex
	data map[string][]byte
}

var _ state.Store = (*Store)(nil)

func New() *Store {
	return &Store{data: make(map[string][]byte)}
}

func FromEntries(entries []model.StateEntry) (*Store, error) {
	store := New()
	for _, entry := range entries {
		key := string(entry.Key)
		if _, exists := store.data[key]; exists {
			return nil, ErrDuplicateKey
		}
		store.data[key] = cloneBytes(entry.Value)
	}
	return store, nil
}

func (s *Store) Get(key []byte) ([]byte, bool) {
	s.mu.RLock()
	value, ok := s.data[string(key)]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return cloneBytes(value), true
}

func (s *Store) Set(key, value []byte) {
	s.mu.Lock()
	s.data[string(key)] = cloneBytes(value)
	s.mu.Unlock()
}

func (s *Store) Delete(key []byte) {
	s.mu.Lock()
	delete(s.data, string(key))
	s.mu.Unlock()
}

func (s *Store) Snapshot() []model.StateEntry {
	s.mu.RLock()
	entries := make([]model.StateEntry, 0, len(s.data))
	for key, value := range s.data {
		entries = append(entries, model.StateEntry{
			Key:   []byte(key),
			Value: cloneBytes(value),
		})
	}
	s.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].Key, entries[j].Key) < 0
	})
	return entries
}

func (s *Store) Clone() *Store {
	clone, err := FromEntries(s.Snapshot())
	if err != nil {
		panic("memkv snapshot unexpectedly contains duplicate keys")
	}
	return clone
}

// ReplaceFrom atomically replaces the receiver with a cloned snapshot.
func (s *Store) ReplaceFrom(source *Store) {
	replacement := source.Snapshot()
	next := make(map[string][]byte, len(replacement))
	for _, entry := range replacement {
		next[string(entry.Key)] = cloneBytes(entry.Value)
	}

	s.mu.Lock()
	s.data = next
	s.mu.Unlock()
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
