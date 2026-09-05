package pkudisk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChunkWriterUploadsOutOfOrderAndCompletes(t *testing.T) {
	const partSize int64 = 8
	payloads := map[int][]byte{
		1: []byte("abcdefgh"),
		2: []byte("ijklmnop"),
		3: []byte("qrst"),
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	var mu sync.Mutex
	gotParts := map[int][]byte{}
	var completeBody map[string]any
	endCalled := false

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case len(r.URL.Path) > len("/part/") && r.URL.Path[:len("/part/")] == "/part/":
			part, err := strconv.Atoi(r.URL.Path[len("/part/"):])
			if err != nil {
				t.Fatal(err)
			}
			now := active.Add(1)
			for {
				old := maxActive.Load()
				if now <= old || maxActive.CompareAndSwap(old, now) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			data, err := io.ReadAll(r.Body)
			active.Add(-1)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotParts[part] = data
			mu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", part))
		case r.URL.Path == "/api/efast/v1/file/oscompleteupload":
			if err := json.NewDecoder(r.Body).Decode(&completeBody); err != nil {
				t.Fatal(err)
			}
			completion := "--boundary\n<CompleteMultipartUpload/>\n--boundary\n" +
				`{"authrequest":["POST","` + server.URL + `/complete","Content-Type: application/xml"]}` +
				"\n--boundary"
			_, _ = w.Write([]byte(completion))
		case r.URL.Path == "/complete":
			body, _ := io.ReadAll(r.Body)
			if string(body) != "<CompleteMultipartUpload/>" {
				t.Fatalf("completion body = %q", body)
			}
		case r.URL.Path == "/api/efast/v1/file/osendupload":
			endCalled = true
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/api/efast/v1/file/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://lib/file", "file_name": "large.bin", "size": 20, "rev": "rev-new",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})
	writer := &pkudiskChunkWriter{
		api:         client,
		objectHTTP:  server.Client(),
		init:        multipartInit{DocID: "gns://lib/file", Rev: "rev-new", UploadID: "upload-1"},
		existingRev: "rev-old",
		size:        20,
		partSize:    partSize,
		partCount:   3,
		signedParts: multipartSignedParts{AuthRequests: map[string][]string{
			"1": {"PUT", server.URL + "/part/1"},
			"2": {"PUT", server.URL + "/part/2"},
			"3": {"PUT", server.URL + "/part/3"},
		}},
		partInfo:    make(map[string][]any),
		resumeState: &multipartResumeState{Completed: make(map[string]multipartResumePart)},
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	for _, chunk := range []int{2, 0, 1} {
		chunk := chunk
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := writer.WriteChunk(ctx, chunk, bytes.NewReader(payloads[chunk+1]))
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() < 2 {
		t.Fatalf("multipart writes did not overlap; max active = %d", maxActive.Load())
	}
	for part, want := range payloads {
		if !bytes.Equal(gotParts[part], want) {
			t.Fatalf("part %d = %q, want %q", part, gotParts[part], want)
		}
		key := strconv.Itoa(part)
		persisted := writer.resumeState.Completed[key]
		sum := sha256.Sum256(want)
		if persisted.Size != int64(len(want)) || persisted.SHA256 != fmt.Sprintf("%x", sum[:]) {
			t.Fatalf("resume digest for part %d = %#v", part, persisted)
		}
	}

	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !endCalled {
		t.Fatal("osendupload was not called")
	}
	partInfo, ok := completeBody["partinfo"].(map[string]any)
	if !ok || len(partInfo) != 3 {
		t.Fatalf("completion partinfo = %#v", completeBody["partinfo"])
	}
	for part, wantSize := range map[string]int64{"1": 8, "2": 8, "3": 4} {
		values, ok := partInfo[part].([]any)
		if !ok || len(values) != 2 {
			t.Fatalf("partinfo[%s] = %#v", part, partInfo[part])
		}
		if values[0] != fmt.Sprintf("\"etag-%s\"", part) {
			t.Fatalf("partinfo[%s] etag = %#v", part, values[0])
		}
		if gotSize, ok := values[1].(float64); !ok || int64(gotSize) != wantSize {
			t.Fatalf("partinfo[%s] size = %#v", part, values[1])
		}
	}
}

func TestChunkWriterRequiresEveryPartBeforeClose(t *testing.T) {
	writer := &pkudiskChunkWriter{
		partCount: 2,
		partInfo: map[string][]any{
			"1": {"etag-1", int64(8)},
		},
	}
	if err := writer.Close(context.Background()); err == nil {
		t.Fatal("Close succeeded with a missing multipart chunk")
	}
}

func TestChunkWriterAbortMakesWriterUnusableWithoutDeleting(t *testing.T) {
	writer := &pkudiskChunkWriter{
		partCount: 1,
		partInfo:  make(map[string][]any),
	}
	if err := writer.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(context.Background()); err == nil {
		t.Fatal("Close succeeded after Abort")
	}
}
