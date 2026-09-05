package pkudisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/fs"
)

const multipartResumeVersion = 2

type multipartResumePart struct {
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

type multipartResumeState struct {
	Version        int                            `json:"version"`
	SourceIdentity string                         `json:"source_identity"`
	Size           int64                          `json:"size"`
	ModTimeNsec    int64                          `json:"mod_time_nsec"`
	ExistingID     string                         `json:"existing_id,omitempty"`
	ExistingRev    string                         `json:"existing_rev,omitempty"`
	PartSize       int64                          `json:"part_size"`
	PartCount      int64                          `json:"part_count"`
	Init           multipartInit                  `json:"init"`
	Completed      map[string]multipartResumePart `json:"completed,omitempty"`
}

// multipartResumeSourceIdentity identifies the source object without storing
// its path or config string in the resume cache. The fast fingerprint follows
// rclone's own change-detection semantics: it includes size/modtime and uses a
// source hash only when the source backend can provide one cheaply. An object
// ID, when available, further distinguishes replacement objects at one path.
func multipartResumeSourceIdentity(ctx context.Context, src fs.ObjectInfo) string {
	h := sha256.New()
	write := func(value string) {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	write(fs.ConfigString(src.Fs()))
	write(src.Remote())
	write(fs.Fingerprint(ctx, src, true))
	if ider, ok := src.(fs.IDer); ok {
		write(ider.ID())
	}
	return hex.EncodeToString(h.Sum(nil))
}

type multipartResumeStore struct {
	statePath   string
	lock        *flock.Flock
	enabled     bool
	releaseOnce sync.Once
}

func (f *Fs) multipartResumeRemote(remote string) string {
	if f.root == "" {
		return path.Clean(remote)
	}
	return path.Join(f.root, remote)
}

func (f *Fs) openMultipartResumeStore(ctx context.Context, remote string) *multipartResumeStore {
	if f.resumeDir == "" {
		return &multipartResumeStore{}
	}
	if err := os.MkdirAll(f.resumeDir, 0o700); err != nil {
		fs.Debugf(f, "multipart resume disabled: create cache directory: %v", err)
		return &multipartResumeStore{}
	}

	identity := f.opt.BaseURL + "\x00" + f.name + "\x00" + f.multipartResumeRemote(remote)
	sum := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(sum[:])
	store := &multipartResumeStore{
		statePath: filepath.Join(f.resumeDir, key+".json"),
	}
	store.lock = flock.New(filepath.Join(f.resumeDir, key+".lock"))
	locked, err := store.lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			fs.Debugf(f, "multipart resume disabled: file locking is unsupported on this platform")
		} else {
			fs.Debugf(f, "multipart resume disabled: acquire state lock: %v", err)
		}
		return &multipartResumeStore{}
	}
	if !locked {
		fs.Debugf(f, "multipart resume disabled: state lock was not acquired")
		return &multipartResumeStore{}
	}
	store.enabled = true
	return store
}

func (s *multipartResumeStore) release() {
	if s == nil || !s.enabled {
		return
	}
	s.releaseOnce.Do(func() {
		if err := s.lock.Unlock(); err != nil {
			fs.Debugf(nil, "release PKU Disk multipart resume lock: %v", err)
		}
	})
}

func (s *multipartResumeStore) load() (*multipartResumeState, error) {
	if s == nil || !s.enabled {
		return nil, nil
	}
	data, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state multipartResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		_ = s.remove()
		return nil, fmt.Errorf("decode multipart resume state: %w", err)
	}
	return &state, nil
}

func (s *multipartResumeStore) save(state *multipartResumeState) error {
	if s == nil || !s.enabled {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.statePath), ".multipart-resume-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.statePath); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return err
	}
	// Some platforms don't replace an existing destination with Rename.
	// Losing the cache file in the tiny remove/rename window only loses resume
	// progress; it cannot make remote metadata authoritative or corrupt a file.
	if err := os.Remove(s.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmpName, s.statePath)
}

func (s *multipartResumeStore) remove() error {
	if s == nil || !s.enabled {
		return nil
	}
	err := os.Remove(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func newMultipartResumeState(sourceIdentity string, size int64, modTime time.Time, existingID, existingRev string, partSize, partCount int64, init multipartInit) *multipartResumeState {
	return &multipartResumeState{
		Version:        multipartResumeVersion,
		SourceIdentity: sourceIdentity,
		Size:           size,
		ModTimeNsec:    modTime.UnixNano(),
		ExistingID:     existingID,
		ExistingRev:    existingRev,
		PartSize:       partSize,
		PartCount:      partCount,
		Init:           init,
		Completed:      make(map[string]multipartResumePart),
	}
}

func (s *multipartResumeState) validFor(sourceIdentity string, size int64, modTime time.Time, existingID, existingRev string, partSize, partCount int64) bool {
	if s == nil || s.Version != multipartResumeVersion ||
		s.SourceIdentity == "" || s.SourceIdentity != sourceIdentity ||
		s.Size != size || s.ModTimeNsec != modTime.UnixNano() ||
		s.ExistingID != existingID || s.ExistingRev != existingRev ||
		s.PartSize != partSize || s.PartCount != partCount ||
		s.Init.DocID == "" || s.Init.Rev == "" || s.Init.UploadID == "" {
		return false
	}
	for key, part := range s.Completed {
		n, err := strconv.ParseInt(key, 10, 64)
		if err != nil || n < 1 || n > partCount || part.ETag == "" {
			return false
		}
		wantSize := min(partSize, size-(n-1)*partSize)
		if part.Size != wantSize {
			return false
		}
	}
	return true
}

func (s *multipartResumeState) partInfo() map[string][]any {
	out := make(map[string][]any, len(s.Completed))
	for key, part := range s.Completed {
		out[key] = []any{part.ETag, part.Size}
	}
	return out
}

func missingMultipartRanges(partCount int64, partInfo map[string][]any) []string {
	var ranges []string
	for part := int64(1); part <= partCount; {
		if _, ok := partInfo[strconv.FormatInt(part, 10)]; ok {
			part++
			continue
		}
		start := part
		for part <= partCount {
			if _, ok := partInfo[strconv.FormatInt(part, 10)]; ok {
				break
			}
			part++
		}
		end := part - 1
		if start == end {
			ranges = append(ranges, strconv.FormatInt(start, 10))
		} else {
			ranges = append(ranges, fmt.Sprintf("%d-%d", start, end))
		}
	}
	return ranges
}
