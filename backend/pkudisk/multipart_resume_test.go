package pkudisk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/object"
)

func testPartSHA256(fill byte, size int64) string {
	h := sha256.New()
	block := bytes.Repeat([]byte{fill}, 1<<20)
	remaining := size
	for remaining > 0 {
		n := min(int64(len(block)), remaining)
		_, _ = h.Write(block[:n])
		remaining -= n
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func writeRepeatedTestFile(t *testing.T, name string, fill byte, size int64, modTime time.Time) {
	t.Helper()
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte{fill}, 1<<20)
	remaining := size
	for remaining > 0 {
		n := min(int64(len(block)), remaining)
		if _, err := f.Write(block[:n]); err != nil {
			f.Close()
			t.Fatal(err)
		}
		remaining -= n
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func newResumeLocalObject(t *testing.T, ctx context.Context, root, remote string) fs.Object {
	t.Helper()
	f, err := local.NewFs(ctx, "local", root, configmap.Simple{})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.NewObject(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestMultipartResumeStateIdentityAndMissingRanges(t *testing.T) {
	modTime := time.Unix(1_800_000_000, 123_456_789)
	state := newMultipartResumeState(
		"source-a",
		45,
		modTime,
		"parent-id",
		"file.bin",
		"old-id",
		"old-rev",
		20,
		3,
		multipartInit{DocID: "upload-doc", Rev: "upload-rev", UploadID: "upload-id"},
	)
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: 20, SHA256: testPartSHA256('a', 20)}
	state.Completed["3"] = multipartResumePart{ETag: "etag-3", Size: 5, SHA256: testPartSHA256('c', 5)}

	if !state.validFor("source-a", 45, modTime, "parent-id", "file.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("valid resume state was rejected")
	}
	state.Version = multipartResumeVersion - 1
	if state.validFor("source-a", 45, modTime, "parent-id", "file.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted an older unsafe state version")
	}
	state.Version = multipartResumeVersion
	if state.validFor("source-b", 45, modTime, "parent-id", "file.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a different source identity")
	}
	if state.validFor("source-a", 45, modTime.Add(time.Nanosecond), "parent-id", "file.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a changed source mtime")
	}
	if state.validFor("source-a", 45, modTime, "parent-id", "file.bin", "old-id", "new-rev", 20, 3) {
		t.Fatal("resume state accepted a changed destination revision")
	}
	if state.validFor("source-a", 45, modTime, "different-parent", "file.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a changed destination parent")
	}
	if state.validFor("source-a", 45, modTime, "parent-id", "other.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a changed encoded destination leaf")
	}
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: 20}
	if state.validFor("source-a", 45, modTime, "parent-id", "file.bin", "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a completed part without a content digest")
	}

	ranges := missingMultipartRanges(3, state.partInfo())
	if len(ranges) != 1 || ranges[0] != "2" {
		t.Fatalf("missing ranges = %v, want [2]", ranges)
	}
}

func TestMultipartResumeSourceIdentityDistinguishesSameSizeAndModTimeSources(t *testing.T) {
	ctx := context.Background()
	modTime := time.Unix(1_800_000_050, 123_456_789)
	srcA := object.NewStaticObjectInfo("A.bin", modTime, 100<<20, true, nil, nil)
	srcB := object.NewStaticObjectInfo("B.bin", modTime, 100<<20, true, nil, nil)

	idA := multipartResumeSourceIdentity(ctx, srcA)
	idB := multipartResumeSourceIdentity(ctx, srcB)
	if idA == idB {
		t.Fatalf("different source paths with identical size/mtime share identity %q", idA)
	}
	if idA != multipartResumeSourceIdentity(ctx, srcA) {
		t.Fatal("source identity is not stable")
	}
}
func TestOpenChunkWriterRejectsInPlaceSourceReplacementWithSameSizeAndModTime(t *testing.T) {
	ctx := context.Background()
	const partSize int64 = defaultMultipartPartSize
	const totalSize int64 = 2 * partSize
	modTime := time.Unix(1_800_000_075, 333_444_555)
	sourceRoot := t.TempDir()
	sourcePath := filepath.Join(sourceRoot, "file.bin")
	writeRepeatedTestFile(t, sourcePath, 'A', totalSize, modTime)
	srcA := newResumeLocalObject(t, ctx, sourceRoot, "file.bin")

	var initCalls int
	var requestedParts string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			writePathNotFound(w)
		case "/api/efast/v1/file/osoption":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"partminsize": 1,
				"partmaxsize": 1 << 30,
				"partmaxnum":  10000,
			})
		case "/api/efast/v1/file/osinitmultiupload":
			initCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid":    "gns://personal/dst/fresh",
				"rev":      "fresh-rev",
				"uploadid": "fresh-upload-id",
			})
		case "/api/efast/v1/file/osuploadpart":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			requestedParts, _ = body["parts"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authrequests": map[string]any{
					"1": []string{"PUT", server.URL + "/part/1"},
					"2": []string{"PUT", server.URL + "/part/2"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	f.resumeDir = t.TempDir()
	remote := "Personal/dst/file.bin"
	state := newMultipartResumeState(
		multipartResumeSourceIdentity(ctx, srcA), totalSize, modTime,
		"gns://personal/dst", "file.bin", "", "", partSize, 2,
		multipartInit{DocID: "gns://personal/dst/old", Rev: "old-rev", UploadID: "old-upload-id"},
	)
	state.Completed["1"] = multipartResumePart{
		ETag:   "etag-old",
		Size:   partSize,
		SHA256: testPartSHA256('A', partSize),
	}
	store := f.openMultipartResumeStore(ctx, remote)
	if !store.enabled {
		t.Fatal("resume store unexpectedly disabled")
	}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	store.release()

	writeRepeatedTestFile(t, sourcePath, 'B', totalSize, modTime)
	srcB := newResumeLocalObject(t, ctx, sourceRoot, "file.bin")
	if got, want := multipartResumeSourceIdentity(ctx, srcB), state.SourceIdentity; got != want {
		t.Fatalf("test precondition failed: metadata identity changed after in-place replacement: got %q want %q", got, want)
	}

	_, writer, err := f.OpenChunkWriter(ctx, remote, srcB)
	if err != nil {
		t.Fatal(err)
	}
	if initCalls != 0 {
		t.Fatalf("fresh multipart init happened before completed part verification: %d calls", initCalls)
	}
	if requestedParts != "2" {
		t.Fatalf("initial resumed signed parts = %q, want only missing part 2", requestedParts)
	}
	changedReader := bytes.NewReader(bytes.Repeat([]byte{'B'}, int(partSize)))
	if _, err := writer.WriteChunk(ctx, 0, changedReader); !fserrors.IsRetryError(err) {
		t.Fatalf("changed completed part error = %v, want retryable error", err)
	}
	if err := writer.Abort(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(store.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed-source resume state still exists after mismatch: %v", err)
	}

	requestedParts = ""
	_, freshWriter, err := f.OpenChunkWriter(ctx, remote, srcB)
	if err != nil {
		t.Fatal(err)
	}
	if initCalls != 1 {
		t.Fatalf("fresh multipart init calls = %d, want 1 after retry", initCalls)
	}
	if requestedParts != "1-2" {
		t.Fatalf("fresh signed parts = %q, want all parts", requestedParts)
	}
	if err := freshWriter.Abort(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOpenChunkWriterVerifiesLocalCompletedPartWithoutConsumingAccountedReader(t *testing.T) {
	ctx := context.Background()
	const partSize int64 = defaultMultipartPartSize
	const totalSize int64 = 2 * partSize
	modTime := time.Unix(1_800_000_100, 222_333_444)
	requestedParts := ""
	sourceRoot := t.TempDir()
	writeRepeatedTestFile(t, filepath.Join(sourceRoot, "file.bin"), 0, totalSize, modTime)
	src := newResumeLocalObject(t, ctx, sourceRoot, "file.bin")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			writePathNotFound(w)
		case "/api/efast/v1/file/osoption":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"partminsize": 1,
				"partmaxsize": 1 << 30,
				"partmaxnum":  10000,
			})
		case "/api/efast/v1/file/osuploadpart":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			requestedParts, _ = body["parts"].(string)
			if requestedParts != "2" {
				t.Fatalf("requested parts = %q, want 2", requestedParts)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authrequests": map[string]any{
					"2": []string{"PUT", server.URL + "/part/2"},
				},
			})
		case "/api/efast/v1/file/osinitmultiupload":
			t.Fatal("resume unexpectedly initialized a new multipart upload")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	f.resumeDir = t.TempDir()
	remote := "Personal/dst/file.bin"
	state := newMultipartResumeState(
		multipartResumeSourceIdentity(ctx, src), totalSize, modTime,
		"gns://personal/dst", "file.bin", "", "", partSize, 2,
		multipartInit{DocID: "gns://personal/dst/incomplete", Rev: "upload-rev", UploadID: "upload-id"},
	)
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: partSize, SHA256: testPartSHA256(0, partSize)}
	store := f.openMultipartResumeStore(ctx, remote)
	if !store.enabled {
		t.Fatal("resume store unexpectedly disabled")
	}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	store.release()

	f.opt.UploadConcurrency = 1

	info, writer, err := f.OpenChunkWriter(ctx, remote, src)
	if err != nil {
		t.Fatal(err)
	}
	if info.LeavePartsOnError {
		t.Fatal("LeavePartsOnError must be false so rclone calls Abort to release the state lock")
	}
	if info.Concurrency != 1 {
		t.Fatalf("chunk writer concurrency = %d, want configured value 1", info.Concurrency)
	}
	if requestedParts != "2" {
		t.Fatalf("OpenChunkWriter requested %q, want only missing part 2", requestedParts)
	}

	reader := bytes.NewReader(make([]byte, partSize))
	written, err := writer.WriteChunk(ctx, 0, reader)
	if err != nil {
		t.Fatal(err)
	}
	if written != partSize || int64(reader.Len()) != partSize {
		t.Fatalf("resumed chunk written=%d accounted reader consumed=%d", written, partSize-int64(reader.Len()))
	}

	if err := writer.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	reopened := f.openMultipartResumeStore(ctx, remote)
	if !reopened.enabled {
		t.Fatal("Abort did not release multipart resume lock")
	}
	defer reopened.release()
	persisted, err := reopened.load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.Completed["1"].ETag != "etag-1" {
		t.Fatalf("Abort did not preserve completed resume state: %#v", persisted)
	}
}

func TestChunkWriterFinalizationFailureDiscardsAmbiguousResumeState(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/efast/v1/file/oscompleteupload" {
			http.Error(w, "completion failed", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	f := &Fs{name: "pku", opt: Options{BaseURL: server.URL}, resumeDir: t.TempDir()}
	remote := "Personal/file.bin"
	store := f.openMultipartResumeStore(ctx, remote)
	if !store.enabled {
		t.Fatal("resume store unexpectedly disabled")
	}
	state := newMultipartResumeState(
		"source", 8, time.Unix(1_800_000_200, 0), "parent-id", "file.bin", "", "", 8, 1,
		multipartInit{DocID: "upload-doc", Rev: "upload-rev", UploadID: "upload-id"},
	)
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: 8, SHA256: testPartSHA256(0, 8)}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}

	writer := &pkudiskChunkWriter{
		api:         mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"}),
		objectHTTP:  server.Client(),
		init:        state.Init,
		size:        8,
		partSize:    8,
		partCount:   1,
		partInfo:    state.partInfo(),
		resumeStore: store,
		resumeState: state,
	}
	if err := writer.Close(ctx); err == nil {
		t.Fatal("expected finalization failure")
	}
	if _, err := store.load(); err != nil {
		t.Fatal(err)
	} else if _, statErr := os.Stat(store.statePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ambiguous resume state was not removed: %v", statErr)
	}

	reopened := f.openMultipartResumeStore(ctx, remote)
	if !reopened.enabled {
		t.Fatal("finalization failure did not release resume lock")
	}
	reopened.release()
}
