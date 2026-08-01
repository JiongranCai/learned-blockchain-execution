package memkv_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

func TestStoreOwnsBytesAndSnapshotsInKeyOrder(t *testing.T) {
	key := []byte("b")
	value := []byte("two")
	store, err := memkv.FromEntries([]model.StateEntry{
		{Key: key, Value: value},
		{Key: []byte("a"), Value: []byte("one")},
	})
	if err != nil {
		t.Fatal(err)
	}
	key[0] = 'x'
	value[0] = 'X'

	got, ok := store.Get([]byte("b"))
	if !ok || !bytes.Equal(got, []byte("two")) {
		t.Fatalf("store did not own constructor bytes: value=%q exists=%v", got, ok)
	}
	got[0] = 'X'
	if again, _ := store.Get([]byte("b")); !bytes.Equal(again, []byte("two")) {
		t.Fatalf("Get exposed store-owned bytes: %q", again)
	}

	want := []model.StateEntry{
		{Key: []byte("a"), Value: []byte("one")},
		{Key: []byte("b"), Value: []byte("two")},
	}
	if snapshot := store.Snapshot(); !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot is not canonical:\n got: %#v\nwant: %#v", snapshot, want)
	}
}

func TestCloneAndReplaceAreIndependent(t *testing.T) {
	original, err := memkv.FromEntries([]model.StateEntry{{Key: []byte("a"), Value: []byte("one")}})
	if err != nil {
		t.Fatal(err)
	}
	clone := original.Clone()
	clone.Set([]byte("a"), []byte("changed"))
	clone.Set([]byte("b"), []byte("two"))

	if got, _ := original.Get([]byte("a")); !bytes.Equal(got, []byte("one")) {
		t.Fatalf("clone mutation affected original: %q", got)
	}
	original.ReplaceFrom(clone)
	clone.Set([]byte("a"), []byte("changed-again"))

	want := []model.StateEntry{
		{Key: []byte("a"), Value: []byte("changed")},
		{Key: []byte("b"), Value: []byte("two")},
	}
	if got := original.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected replacement:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestFromEntriesRejectsDuplicateKeys(t *testing.T) {
	_, err := memkv.FromEntries([]model.StateEntry{
		{Key: []byte("same"), Value: []byte("one")},
		{Key: []byte("same"), Value: []byte("two")},
	})
	if !errors.Is(err, memkv.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
}

func TestReplaceRejectsDuplicatesWithoutPublishing(t *testing.T) {
	store, err := memkv.FromEntries([]model.StateEntry{
		{Key: []byte("stable"), Value: []byte("before")},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	err = store.Replace([]model.StateEntry{
		{Key: []byte("same"), Value: []byte("one")},
		{Key: []byte("same"), Value: []byte("two")},
	})
	if !errors.Is(err, memkv.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
	if got := store.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed replacement changed state: got %#v want %#v", got, before)
	}
}
