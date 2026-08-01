package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
)

const canonicalResultSchema = "block-result-v1"

// CanonicalDigest hashes result-visible fields using explicit length prefixes
// and stable state/write ordering. The Digest field itself is excluded.
func CanonicalDigest(result BlockResult) string {
	h := sha256.New()
	writeString(h, canonicalResultSchema)
	writeString(h, result.BlockID)
	writeUint64(h, result.Height)

	writeUint64(h, uint64(len(result.Transactions)))
	for _, tx := range result.Transactions {
		writeUint64(h, tx.Index)
		writeString(h, tx.TransactionID)
		writeString(h, string(tx.Status))
		writeBytes(h, tx.ReturnValue)
		writeString(h, tx.ErrorCode)
		writeUint64(h, tx.UnitsUsed)
		writeUint64(h, tx.ComputeDigest)

		writeUint64(h, uint64(len(tx.Reads)))
		for _, read := range tx.Reads {
			writeBytes(h, read.Key)
			writeBool(h, read.Exists)
			writeBytes(h, read.Value)
		}

		writes := append([]StateChange(nil), tx.Writes...)
		sort.Slice(writes, func(i, j int) bool {
			if comparison := bytes.Compare(writes[i].Key, writes[j].Key); comparison != 0 {
				return comparison < 0
			}
			if writes[i].Delete != writes[j].Delete {
				return !writes[i].Delete
			}
			return bytes.Compare(writes[i].Value, writes[j].Value) < 0
		})
		writeUint64(h, uint64(len(writes)))
		for _, write := range writes {
			writeBytes(h, write.Key)
			writeBool(h, write.Delete)
			writeBytes(h, write.Value)
		}

		writeUint64(h, uint64(len(tx.Events)))
		for _, event := range tx.Events {
			writeString(h, event.Type)
			writeBytes(h, event.Data)
		}
	}

	state := append([]StateEntry(nil), result.FinalState...)
	sort.Slice(state, func(i, j int) bool {
		if comparison := bytes.Compare(state[i].Key, state[j].Key); comparison != 0 {
			return comparison < 0
		}
		return bytes.Compare(state[i].Value, state[j].Value) < 0
	})
	writeUint64(h, uint64(len(state)))
	for _, entry := range state {
		writeBytes(h, entry.Key)
		writeBytes(h, entry.Value)
	}

	return hex.EncodeToString(h.Sum(nil))
}

func writeBool(h hash.Hash, value bool) {
	if value {
		_, _ = h.Write([]byte{1})
		return
	}
	_, _ = h.Write([]byte{0})
}

func writeString(h hash.Hash, value string) {
	writeBytes(h, []byte(value))
}

func writeBytes(h hash.Hash, value []byte) {
	writeUint64(h, uint64(len(value)))
	_, _ = h.Write(value)
}

func writeUint64(h hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = h.Write(encoded[:])
}
