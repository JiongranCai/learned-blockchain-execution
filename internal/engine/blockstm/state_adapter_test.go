package blockstm

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	storetypes "cosmossdk.io/store/types"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

func TestValueFramesPreserveExistingEmptyValues(t *testing.T) {
	for _, value := range [][]byte{nil, {}, {0}, []byte("payload")} {
		encoded := encodeValue(value)
		if len(encoded) == 0 || encoded[0] != valueFrameMarker {
			t.Fatalf("value was not framed: %x", encoded)
		}
		decoded, err := decodeValue(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, value) {
			t.Fatalf("frame round trip changed bytes: got %x want %x", decoded, value)
		}
	}
	for _, encoded := range [][]byte{nil, {}, {0}, {2, 1}} {
		if _, err := decodeValue(encoded); !errors.Is(err, ErrInvalidValueFrame) {
			t.Fatalf("invalid frame %x returned %v", encoded, err)
		}
	}
}

func TestKernelStorageRoundTripAndInvalidFrame(t *testing.T) {
	key := storetypes.NewKVStoreKey("adapter-test")
	stores := map[storetypes.StoreKey]int{key: 0}
	want := []model.StateEntry{
		{Key: []byte("a-empty"), Value: nil},
		{Key: []byte("b-value"), Value: []byte("value")},
	}
	storage := newKernelStorage(stores, key, []model.StateEntry{want[1], want[0]})
	view := framedState{store: storage.GetKVStore(key)}
	if value, exists := view.Get([]byte("a-empty")); !exists || len(value) != 0 {
		t.Fatalf("existing empty value collapsed into missing: value=%x exists=%v", value, exists)
	}
	if _, exists := view.Get([]byte("missing")); exists {
		t.Fatal("missing key reported as existing")
	}
	got, err := snapshotKernelStorage(storage, key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected adapter snapshot: got %#v want %#v", got, want)
	}

	storage.GetKVStore(key).Set([]byte("corrupt"), []byte{2})
	if _, err := snapshotKernelStorage(storage, key); !errors.Is(err, ErrInvalidValueFrame) {
		t.Fatalf("corrupt adapter state returned %v", err)
	}
}
