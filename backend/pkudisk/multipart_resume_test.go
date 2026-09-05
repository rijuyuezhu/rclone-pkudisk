package pkudisk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/object"
)

func TestMultipartResumeStateIdentityAndMissingRanges(t *testing.T) {
	modTime := time.Unix(1_800_000_000, 123_456_789)
	state := newMultipartResumeState(
		"source-a",
		45,
		modTime,
		"old-id",
		"old-rev",
		20,
		3,
		multipartInit{DocID: "upload-doc", Rev: "upload-rev", UploadID: "upload-id"},
	)
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: 20}
	state.Completed["3"] = multipartResumePart{ETag: "etag-3", Size: 5}

	if !state.validFor("source-a", 45, modTime, "old-id", "old-rev", 20, 3) {
		t.Fatal("valid resume state was rejected")
	}
	state.Version = multipartResumeVersion - 1
	if state.validFor("source-a", 45, modTime, "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted an older unsafe state version")
	}
	state.Version = multipartResumeVersion
	if state.validFor("source-b", 45, modTime, "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a different source identity")
	}
	if state.validFor("source-a", 45, modTime.Add(time.Nanosecond), "old-id", "old-rev", 20, 3) {
		t.Fatal("resume state accepted a changed source mtime")
	}
	if state.validFor("source-a", 45, modTime, "old-id", "new-rev", 20, 3) {
		t.Fatal("resume state accepted a changed destination revision")
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

type unreadResumeReader struct {
	reads int
	seeks int
}

func (r *unreadResumeReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("resumed chunk unexpectedly read source")
}

func (r *unreadResumeReader) Seek(int64, int) (int64, error) {
	r.seeks++
	return 0, errors.New("resumed chunk unexpectedly sought source")
}

func TestOpenChunkWriterResumesCompletedPartWithoutReadingSource(t *testing.T) {
	ctx := context.Background()
	const partSize int64 = defaultMultipartPartSize
	const totalSize int64 = 2 * partSize
	modTime := time.Unix(1_800_000_100, 222_333_444)
	requestedParts := ""

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
	src := object.NewStaticObjectInfo("file.bin", modTime, totalSize, true, nil, nil)
	state := newMultipartResumeState(
		multipartResumeSourceIdentity(ctx, src), totalSize, modTime, "", "", partSize, 2,
		multipartInit{DocID: "gns://personal/dst/incomplete", Rev: "upload-rev", UploadID: "upload-id"},
	)
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: partSize}
	store := f.openMultipartResumeStore(ctx, remote)
	if !store.enabled {
		t.Fatal("resume store unexpectedly disabled")
	}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	store.release()

	info, writer, err := f.OpenChunkWriter(ctx, remote, src)
	if err != nil {
		t.Fatal(err)
	}
	if info.LeavePartsOnError {
		t.Fatal("LeavePartsOnError must be false so rclone calls Abort to release the state lock")
	}
	if requestedParts != "2" {
		t.Fatalf("OpenChunkWriter requested %q, want only missing part 2", requestedParts)
	}

	reader := new(unreadResumeReader)
	written, err := writer.WriteChunk(ctx, 0, reader)
	if err != nil {
		t.Fatal(err)
	}
	if written != partSize || reader.reads != 0 || reader.seeks != 0 {
		t.Fatalf("resumed chunk written=%d reads=%d seeks=%d", written, reader.reads, reader.seeks)
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
		"source", 8, time.Unix(1_800_000_200, 0), "", "", 8, 1,
		multipartInit{DocID: "upload-doc", Rev: "upload-rev", UploadID: "upload-id"},
	)
	state.Completed["1"] = multipartResumePart{ETag: "etag-1", Size: 8}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}

	writer := &pkudiskChunkWriter{
		api:         newAPIClient(server.URL, &staticTokenProvider{token: "test-token"}),
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
