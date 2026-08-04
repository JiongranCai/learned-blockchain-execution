package blockstm

import (
	"errors"

	storetypes "cosmossdk.io/store/types"
	blockstm "github.com/crypto-org-chain/go-block-stm"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

const valueFrameMarker byte = 1

var ErrInvalidValueFrame = errors.New("invalid Block-STM adapter value frame")

// framedState preserves the distinction between a missing key and an existing
// nil/empty value. Cosmos KVStore reserves nil for deletion, while the serial
// oracle permits arbitrary bytes and classifies non-eight-byte values at the
// runtime layer.
type framedState struct {
	store storetypes.KVStore
}

func (s framedState) Get(key []byte) ([]byte, bool) {
	encoded := s.store.Get(key)
	if encoded == nil {
		return nil, false
	}
	value, err := decodeValue(encoded)
	if err != nil {
		// All values in the private adapter store are produced by encodeValue.
		// Return an invalid runtime value if that invariant is ever violated;
		// snapshot decoding will surface the infrastructure error before publish.
		return nil, true
	}
	return value, true
}

func (s framedState) Set(key, value []byte) {
	s.store.Set(key, encodeValue(value))
}

func (s framedState) Delete(key []byte) {
	s.store.Delete(key)
}

func newKernelStorage(
	stores map[storetypes.StoreKey]int,
	key storetypes.StoreKey,
	entries []model.StateEntry,
) *blockstm.MultiMemDB {
	storage := blockstm.NewMultiMemDB(stores)
	view := framedState{store: storage.GetKVStore(key)}
	for _, entry := range entries {
		view.Set(entry.Key, entry.Value)
	}
	return storage
}

func snapshotKernelStorage(storage blockstm.MultiStore, key storetypes.StoreKey) ([]model.StateEntry, error) {
	iterator := storage.GetKVStore(key).Iterator(nil, nil)
	entries := make([]model.StateEntry, 0)
	for ; iterator.Valid(); iterator.Next() {
		value, err := decodeValue(iterator.Value())
		if err != nil {
			_ = iterator.Close()
			return nil, err
		}
		entries = append(entries, model.StateEntry{
			Key:   append([]byte(nil), iterator.Key()...),
			Value: value,
		})
	}
	if err := iterator.Close(); err != nil {
		return nil, err
	}
	return entries, nil
}

func encodeValue(value []byte) []byte {
	encoded := make([]byte, len(value)+1)
	encoded[0] = valueFrameMarker
	copy(encoded[1:], value)
	return encoded
}

func decodeValue(encoded []byte) ([]byte, error) {
	if len(encoded) == 0 || encoded[0] != valueFrameMarker {
		return nil, ErrInvalidValueFrame
	}
	return append([]byte(nil), encoded[1:]...), nil
}
