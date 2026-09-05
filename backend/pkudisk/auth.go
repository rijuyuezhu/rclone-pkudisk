package pkudisk

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/syndtr/goleveldb/leveldb"
)

const (
	defaultLevelDBRelative = ".config/AnyShare/Local Storage/leveldb"
	levelDBEnv             = "PKUDISK_PKUDIST_LEVELDB_DIR"
	levelDBLogBlockSize    = 32_768
)

var (
	tokenKey    = []byte("_file://\x00\x01oauth2Token")
	signedInKey = []byte("_file://\x00\x01richClientIsSignin")
)

type tokenProvider interface {
	Token(context.Context, bool) (string, error)
}

type staticTokenProvider struct {
	token string
}

func (p *staticTokenProvider) Token(context.Context, bool) (string, error) {
	if p.token == "" {
		return "", errors.New("empty PKU Disk access token")
	}
	return p.token, nil
}

type pkudistTokenProvider struct {
	levelDBPath string
	mu          sync.RWMutex
	token       string
}

func newPkudistTokenProvider(configured string) (*pkudistTokenProvider, error) {
	path := configured
	if path == "" {
		path = os.Getenv(levelDBEnv)
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find home directory: %w", err)
		}
		path = filepath.Join(home, defaultLevelDBRelative)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve pkudist LevelDB path: %w", err)
	}
	p := &pkudistTokenProvider{levelDBPath: path}
	if _, err := p.Token(context.Background(), true); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *pkudistTokenProvider) Token(_ context.Context, refresh bool) (string, error) {
	if !refresh {
		p.mu.RLock()
		token := p.token
		p.mu.RUnlock()
		if token != "" {
			return token, nil
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && !refresh {
		return p.token, nil
	}
	token, err := readPkudistAccessToken(p.levelDBPath)
	if err != nil {
		return "", err
	}
	p.token = token
	return token, nil
}

type levelDBMutation struct {
	sequence uint64
	value    []byte
}

func readPkudistAccessToken(dbPath string) (string, error) {
	info, err := os.Stat(dbPath)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("pkudist Local Storage not found at %s; sign in with the PKU Disk desktop client first", dbPath)
	}

	// Chromium may keep the newest OAuth token only in the active LevelDB WAL.
	// Read that first instead of trusting a copied MANIFEST/SST snapshot to have
	// incorporated the latest write batch.
	wal := readLatestWALMutations(dbPath, [][]byte{tokenKey, signedInKey})
	if mutation, ok := wal[string(signedInKey)]; ok {
		if mutation.value == nil {
			return "", errors.New("pkudist is currently signed out")
		}
		signedIn, err := decodeLocalStorageString(mutation.value)
		if err != nil {
			return "", fmt.Errorf("decode pkudist sign-in state: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(signedIn), "false") {
			return "", errors.New("pkudist is currently signed out")
		}
	}
	if mutation, ok := wal[string(tokenKey)]; ok {
		if mutation.value == nil {
			return "", errors.New("pkudist is currently signed out; OAuth token was removed")
		}
		return parseAccessToken(mutation.value)
	}

	// A stable token may already have been compacted into an SSTable. Opening
	// the live DB would contend with Chromium's LOCK, so use a private snapshot.
	tmp, err := os.MkdirTemp("", "rclone-pkudisk-leveldb-")
	if err != nil {
		return "", fmt.Errorf("create LevelDB snapshot: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := copyLevelDBSnapshot(dbPath, tmp); err != nil {
		return "", fmt.Errorf("snapshot pkudist Local Storage: %w", err)
	}
	db, err := leveldb.OpenFile(tmp, nil)
	if err != nil {
		return "", fmt.Errorf("open pkudist Local Storage snapshot: %w", err)
	}
	defer db.Close()

	if raw, err := db.Get(signedInKey, nil); err == nil {
		signedIn, err := decodeLocalStorageString(raw)
		if err != nil {
			return "", fmt.Errorf("decode pkudist sign-in state: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(signedIn), "false") {
			return "", errors.New("pkudist is currently signed out")
		}
	}
	raw, err := db.Get(tokenKey, nil)
	if err != nil {
		return "", errors.New("pkudist has no current OAuth token; sign in with the PKU Disk desktop client first")
	}
	return parseAccessToken(raw)
}

func parseAccessToken(raw []byte) (string, error) {
	text, err := decodeLocalStorageString(raw)
	if err != nil {
		return "", fmt.Errorf("decode pkudist OAuth token: %w", err)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil || payload.AccessToken == "" {
		return "", errors.New("pkudist OAuth token is malformed")
	}
	return payload.AccessToken, nil
}

func readLatestWALMutations(dbPath string, keys [][]byte) map[string]levelDBMutation {
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[string(key)] = struct{}{}
	}
	paths, _ := filepath.Glob(filepath.Join(dbPath, "*.log"))
	sort.Strings(paths)
	result := make(map[string]levelDBMutation)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, record := range levelDBLogicalRecords(data) {
			if len(record) < 12 {
				continue
			}
			sequence := binary.LittleEndian.Uint64(record[:8])
			count := binary.LittleEndian.Uint32(record[8:12])
			pos := 12
			valid := true
			for index := uint32(0); index < count && valid; index++ {
				if pos >= len(record) {
					break
				}
				op := record[pos]
				pos++
				keyLen, next, ok := readLevelDBVarint(record, pos)
				if !ok || next+int(keyLen) > len(record) {
					break
				}
				pos = next
				key := record[pos : pos+int(keyLen)]
				pos += int(keyLen)
				var value []byte
				switch op {
				case 0: // deletion
					value = nil
				case 1: // value
					valueLen, next, ok := readLevelDBVarint(record, pos)
					if !ok || next+int(valueLen) > len(record) {
						valid = false
						continue
					}
					pos = next
					value = append([]byte(nil), record[pos:pos+int(valueLen)]...)
					pos += int(valueLen)
				default:
					valid = false
					continue
				}
				keyString := string(key)
				if _, ok := wanted[keyString]; !ok {
					continue
				}
				mutation := levelDBMutation{sequence: sequence + uint64(index), value: value}
				previous, exists := result[keyString]
				if !exists || mutation.sequence > previous.sequence {
					result[keyString] = mutation
				}
			}
		}
	}
	return result
}

func levelDBLogicalRecords(data []byte) [][]byte {
	var records [][]byte
	var fragment []byte
	for blockStart := 0; blockStart < len(data); blockStart += levelDBLogBlockSize {
		blockEnd := min(blockStart+levelDBLogBlockSize, len(data))
		block := data[blockStart:blockEnd]
		for pos := 0; pos+7 <= len(block); {
			length := int(binary.LittleEndian.Uint16(block[pos+4 : pos+6]))
			recordType := block[pos+6]
			pos += 7
			if length == 0 && recordType == 0 {
				break
			}
			if pos+length > len(block) {
				break
			}
			payload := block[pos : pos+length]
			pos += length
			switch recordType {
			case 1: // FULL
				fragment = nil
				records = append(records, append([]byte(nil), payload...))
			case 2: // FIRST
				fragment = append(fragment[:0], payload...)
			case 3: // MIDDLE
				if fragment != nil {
					fragment = append(fragment, payload...)
				}
			case 4: // LAST
				if fragment != nil {
					fragment = append(fragment, payload...)
					records = append(records, append([]byte(nil), fragment...))
					fragment = nil
				}
			}
		}
	}
	return records
}

func readLevelDBVarint(data []byte, pos int) (uint64, int, bool) {
	var value uint64
	for shift := uint(0); pos < len(data) && shift <= 63; shift += 7 {
		b := data[pos]
		pos++
		value |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return value, pos, true
		}
	}
	return 0, pos, false
}

func copyLevelDBSnapshot(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		iLog := filepath.Ext(entries[i].Name()) == ".log"
		jLog := filepath.Ext(entries[j].Name()) == ".log"
		if iLog != jLog {
			return !iLog
		}
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "LOCK" || !entry.Type().IsRegular() {
			continue
		}
		in, err := os.Open(filepath.Join(src, entry.Name()))
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dst, entry.Name()), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		inErr := in.Close()
		outErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if inErr != nil {
			return inErr
		}
		if outErr != nil {
			return outErr
		}
	}
	return nil
}

func decodeLocalStorageString(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	switch raw[0] {
	case 1:
		return string(raw[1:]), nil
	case 0:
		encoded := raw[1:]
		if len(encoded)%2 != 0 {
			return "", errors.New("malformed UTF-16 Local Storage value")
		}
		units := make([]uint16, len(encoded)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(encoded[i*2 : i*2+2])
		}
		return string(utf16.Decode(units)), nil
	default:
		return string(raw), nil
	}
}
