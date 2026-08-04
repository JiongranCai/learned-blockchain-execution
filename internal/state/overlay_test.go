package state_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/model"
	"github.com/crypto-org-chain/go-block-stm/internal/state"
	"github.com/crypto-org-chain/go-block-stm/internal/state/memkv"
)

func TestOverlayReadsOwnWritesAndCommitsInKeyOrder(t *testing.T) {
	base, err := memkv.FromEntries([]model.StateEntry{
		{Key: []byte("a"), Value: []byte("old")},
		{Key: []byte("z"), Value: []byte("remove")},
	})
	if err != nil {
		t.Fatal(err)
	}
	view := state.NewOverlay(base)

	key := []byte("b")
	value := []byte("new")
	view.Set(key, value)
	view.Set([]byte("a"), []byte("updated"))
	view.Delete([]byte("z"))
	key[0] = 'x'
	value[0] = 'X'

	if got, ok := view.Get([]byte("b")); !ok || !bytes.Equal(got, []byte("new")) {
		t.Fatalf("overlay did not own input bytes: value=%q exists=%v", got, ok)
	}
	if _, ok := view.Get([]byte("z")); ok {
		t.Fatal("overlay delete was not visible to the transaction")
	}
	if got, _ := base.Get([]byte("a")); !bytes.Equal(got, []byte("old")) {
		t.Fatalf("base changed before commit: %q", got)
	}
	if got, _ := base.Get([]byte("z")); !bytes.Equal(got, []byte("remove")) {
		t.Fatalf("base delete escaped before commit: %q", got)
	}

	wantChanges := []model.StateChange{
		{Key: []byte("a"), Value: []byte("updated")},
		{Key: []byte("b"), Value: []byte("new")},
		{Key: []byte("z"), Delete: true},
	}
	changes := view.Changes()
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Fatalf("unexpected sorted changes:\n got: %#v\nwant: %#v", changes, wantChanges)
	}
	changes[0].Value[0] = 'X'
	if got, _ := view.Get([]byte("a")); !bytes.Equal(got, []byte("updated")) {
		t.Fatalf("Changes exposed overlay-owned bytes: %q", got)
	}

	view.CommitTo(base)
	wantState := []model.StateEntry{
		{Key: []byte("a"), Value: []byte("updated")},
		{Key: []byte("b"), Value: []byte("new")},
	}
	if got := base.Snapshot(); !reflect.DeepEqual(got, wantState) {
		t.Fatalf("unexpected committed state:\n got: %#v\nwant: %#v", got, wantState)
	}
}
