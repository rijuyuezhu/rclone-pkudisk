package pkudisk

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func appendVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}

func appendWriteBatchValue(dst, key, value []byte) []byte {
	dst = append(dst, 1)
	dst = appendVarint(dst, uint64(len(key)))
	dst = append(dst, key...)
	dst = appendVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func TestReadLatestWALMutations(t *testing.T) {
	dir := t.TempDir()
	tokenJSON, err := json.Marshal(map[string]string{"access_token": "current-token"})
	if err != nil {
		t.Fatal(err)
	}
	tokenValue := append([]byte{1}, tokenJSON...)
	signedValue := append([]byte{1}, []byte("true")...)

	record := make([]byte, 12)
	binary.LittleEndian.PutUint64(record[:8], 42)
	binary.LittleEndian.PutUint32(record[8:12], 2)
	record = appendWriteBatchValue(record, signedInKey, signedValue)
	record = appendWriteBatchValue(record, tokenKey, tokenValue)

	physical := make([]byte, 7)
	binary.LittleEndian.PutUint16(physical[4:6], uint16(len(record)))
	physical[6] = 1 // FULL
	physical = append(physical, record...)
	if err := os.WriteFile(filepath.Join(dir, "000001.log"), physical, 0o600); err != nil {
		t.Fatal(err)
	}

	mutations := readLatestWALMutations(dir, [][]byte{tokenKey, signedInKey})
	got := mutations[string(tokenKey)]
	if got.sequence != 43 {
		t.Fatalf("token sequence = %d, want 43", got.sequence)
	}
	token, err := parseAccessToken(got.value)
	if err != nil {
		t.Fatal(err)
	}
	if token != "current-token" {
		t.Fatalf("unexpected token %q", token)
	}
}

func TestDecodeLocalStorageString(t *testing.T) {
	got, err := decodeLocalStorageString(append([]byte{1}, []byte("hello")...))
	if err != nil || got != "hello" {
		t.Fatalf("UTF-8 decode = %q, %v", got, err)
	}
	units := utf16.Encode([]rune("北大"))
	raw := []byte{0}
	for _, unit := range units {
		var pair [2]byte
		binary.LittleEndian.PutUint16(pair[:], unit)
		raw = append(raw, pair[:]...)
	}
	got, err = decodeLocalStorageString(raw)
	if err != nil || got != "北大" {
		t.Fatalf("UTF-16 decode = %q, %v", got, err)
	}
}
