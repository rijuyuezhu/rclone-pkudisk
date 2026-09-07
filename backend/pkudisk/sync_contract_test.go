package pkudisk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
)

func TestObjectExposesIDAndRevisionMetadata(t *testing.T) {
	ctx := context.Background()
	o := &Object{id: "gns://personal/file", rev: "rev-123"}

	if got := o.ID(); got != "gns://personal/file" {
		t.Fatalf("ID() = %q, want %q", got, "gns://personal/file")
	}
	metadata, err := o.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := metadata[metadataRevisionKey]; got != "rev-123" {
		t.Fatalf("metadata[%q] = %q, want %q", metadataRevisionKey, got, "rev-123")
	}

	o.rev = ""
	metadata, err = o.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if metadata != nil {
		t.Fatalf("metadata without revision = %#v, want nil", metadata)
	}
}

func TestRegisteredRevisionMetadataIsReadOnly(t *testing.T) {
	info, err := fs.Find("pkudisk")
	if err != nil {
		t.Fatal(err)
	}
	if info.MetadataInfo == nil {
		t.Fatal("pkudisk registration has no metadata info")
	}
	help, ok := info.MetadataInfo.System[metadataRevisionKey]
	if !ok {
		t.Fatalf("pkudisk metadata info is missing %q", metadataRevisionKey)
	}
	if !help.ReadOnly {
		t.Fatalf("pkudisk metadata %q must be read-only", metadataRevisionKey)
	}
}

func TestExtractSyncUploadPreconditions(t *testing.T) {
	tests := []struct {
		name      string
		options   []fs.OpenOption
		want      syncUploadPreconditions
		wantError bool
	}{
		{name: "none"},
		{
			name: "pair",
			options: []fs.OpenOption{fs.MetadataOption{
				syncExpectedIDMetadataKey:  "gns://personal/file",
				syncExpectedRevMetadataKey: "rev-a",
			}},
			want: syncUploadPreconditions{
				expectedID:  "gns://personal/file",
				expectedRev: "rev-a",
				set:         true,
			},
		},
		{
			name: "unrelated metadata ignored",
			options: []fs.OpenOption{fs.MetadataOption{
				"unrelated": "value",
			}},
		},
		{
			name: "missing revision",
			options: []fs.OpenOption{fs.MetadataOption{
				syncExpectedIDMetadataKey: "gns://personal/file",
			}},
			wantError: true,
		},
		{
			name: "missing id",
			options: []fs.OpenOption{fs.MetadataOption{
				syncExpectedRevMetadataKey: "rev-a",
			}},
			wantError: true,
		},
		{
			name: "conflicting duplicate",
			options: []fs.OpenOption{
				fs.MetadataOption{syncExpectedIDMetadataKey: "gns://personal/a", syncExpectedRevMetadataKey: "rev-a"},
				fs.MetadataOption{syncExpectedIDMetadataKey: "gns://personal/b", syncExpectedRevMetadataKey: "rev-a"},
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := extractSyncUploadPreconditions(test.options)
			if test.wantError {
				if err == nil {
					t.Fatal("expected precondition parsing error")
				}
				if !fserrors.IsNoRetryError(err) {
					t.Fatalf("error = %v, want NoRetryError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("preconditions = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestExtractSyncDownloadPreconditions(t *testing.T) {
	options := []fs.OpenOption{
		&fs.HTTPOption{Key: strings.ToLower(syncExpectedIDDownloadHeader), Value: "gns://personal/file"},
		&fs.HTTPOption{Key: syncExpectedRevDownloadHeader, Value: "rev-a"},
		&fs.HTTPOption{Key: "X-Unrelated", Value: "keep-me"},
		&fs.RangeOption{Start: 0, End: 9},
	}
	got, forwarded, err := extractSyncDownloadPreconditions(options)
	if err != nil {
		t.Fatal(err)
	}
	want := syncDownloadPreconditions{expectedID: "gns://personal/file", expectedRev: "rev-a", set: true}
	if got != want {
		t.Fatalf("download preconditions = %#v, want %#v", got, want)
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded options = %#v, want unrelated header + range", forwarded)
	}
	if header, ok := forwarded[0].(*fs.HTTPOption); !ok || header.Key != "X-Unrelated" || header.Value != "keep-me" {
		t.Fatalf("first forwarded option = %#v", forwarded[0])
	}
	if _, ok := forwarded[1].(*fs.RangeOption); !ok {
		t.Fatalf("second forwarded option = %#v, want RangeOption", forwarded[1])
	}

	_, _, err = extractSyncDownloadPreconditions([]fs.OpenOption{
		&fs.HTTPOption{Key: syncExpectedIDDownloadHeader, Value: "gns://personal/file"},
	})
	if err == nil || !fserrors.IsNoRetryError(err) {
		t.Fatalf("missing download revision error = %v, want NoRetryError", err)
	}

	_, _, err = extractSyncDownloadPreconditions([]fs.OpenOption{
		&fs.HTTPOption{Key: syncExpectedIDDownloadHeader, Value: "gns://personal/a"},
		&fs.HTTPOption{Key: syncExpectedIDDownloadHeader, Value: "gns://personal/b"},
		&fs.HTTPOption{Key: syncExpectedRevDownloadHeader, Value: "rev-a"},
	})
	if err == nil || !fserrors.IsNoRetryError(err) {
		t.Fatalf("conflicting download ID error = %v, want NoRetryError", err)
	}
}

func TestOpenUsesExactDownloadRevisionAndConsumesSyncHeaders(t *testing.T) {
	ctx := context.Background()
	payload := "exact revision bytes"
	osDownloadCalls := 0
	var osDownloadBody map[string]any
	var objectHeaders http.Header
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osdownload":
			osDownloadCalls++
			_ = json.NewDecoder(r.Body).Decode(&osDownloadBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authrequest": []any{"GET", server.URL + "/object"},
			})
		case "/object":
			objectHeaders = r.Header.Clone()
			_, _ = w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	api := mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"})
	api.objectHTTP = server.Client()
	f := &Fs{name: "pku", opt: Options{Enc: defaultEncoding}, api: api}
	o := &Object{
		fs:      f,
		remote:  "Personal/file.txt",
		id:      "gns://personal/file",
		size:    int64(len(payload)),
		modTime: time.Unix(1, 0),
		rev:     "rev-current",
	}

	staleOptions := []fs.OpenOption{
		&fs.HTTPOption{Key: syncExpectedIDDownloadHeader, Value: o.id},
		&fs.HTTPOption{Key: syncExpectedRevDownloadHeader, Value: "rev-stale"},
	}
	_, err := o.Open(ctx, staleOptions...)
	if err == nil || !fserrors.IsNoRetryError(err) {
		t.Fatalf("stale download error = %v, want NoRetryError", err)
	}
	if osDownloadCalls != 0 {
		t.Fatalf("stale download made %d osdownload calls", osDownloadCalls)
	}

	options := []fs.OpenOption{
		&fs.HTTPOption{Key: syncExpectedIDDownloadHeader, Value: o.id},
		&fs.HTTPOption{Key: syncExpectedRevDownloadHeader, Value: o.rev},
		&fs.HTTPOption{Key: "X-Unrelated", Value: "forwarded"},
	}
	r, err := o.Open(ctx, options...)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, r); err != nil {
		t.Fatal(err)
	}
	if buf.String() != payload {
		t.Fatalf("downloaded %q, want %q", buf.String(), payload)
	}
	if osDownloadCalls != 1 || osDownloadBody["docid"] != o.id || osDownloadBody["rev"] != o.rev {
		t.Fatalf("osdownload body = %#v, calls=%d", osDownloadBody, osDownloadCalls)
	}
	if objectHeaders.Get(syncExpectedIDDownloadHeader) != "" || objectHeaders.Get(syncExpectedRevDownloadHeader) != "" {
		t.Fatalf("sync download headers leaked to object storage: %#v", objectHeaders)
	}
	if got := objectHeaders.Get("X-Unrelated"); got != "forwarded" {
		t.Fatalf("unrelated header = %q, want forwarded", got)
	}
}

func TestUpdateRejectsStaleSyncPreconditionsBeforeIO(t *testing.T) {
	ctx := context.Background()
	src := object.NewStaticObjectInfo("file.txt", time.Unix(1, 0), 1, true, nil, nil)

	tests := []struct {
		name string
		o    *Object
		meta fs.MetadataOption
	}{
		{
			name: "id changed",
			o:    &Object{id: "gns://personal/current", rev: "rev-a"},
			meta: fs.MetadataOption{
				syncExpectedIDMetadataKey:  "gns://personal/baseline",
				syncExpectedRevMetadataKey: "rev-a",
			},
		},
		{
			name: "revision changed",
			o:    &Object{id: "gns://personal/file", rev: "rev-b"},
			meta: fs.MetadataOption{
				syncExpectedIDMetadataKey:  "gns://personal/file",
				syncExpectedRevMetadataKey: "rev-a",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.o.Update(ctx, strings.NewReader("x"), src, test.meta)
			if err == nil {
				t.Fatal("expected stale precondition error")
			}
			if !fserrors.IsNoRetryError(err) {
				t.Fatalf("error = %v, want NoRetryError", err)
			}
			if !strings.Contains(err.Error(), "sync precondition failed") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestUpdateUsesExpectedRevisionForUpload(t *testing.T) {
	ctx := context.Background()
	var beginBody map[string]any
	var endBody map[string]any
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/osbeginupload":
			_ = json.NewDecoder(r.Body).Decode(&beginBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/file", "rev": "rev-new",
				"authrequest": []string{"POST", server.URL + "/object-upload"},
			})
		case "/object-upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/efast/v1/file/osendupload":
			_ = json.NewDecoder(r.Body).Decode(&endBody)
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/file", "file_name": "file.txt", "size": 7,
				"rev": "rev-new", "client_mtime": int64(1_609_646_706_654_321),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{
		name: "pku",
		opt:  Options{Enc: defaultEncoding},
		api:  mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"}),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")

	mtime := time.Unix(0, 1_609_646_706_654_321*int64(time.Microsecond)).UTC()
	o := &Object{
		fs:      f,
		remote:  "Personal/file.txt",
		id:      "gns://personal/file",
		size:    3,
		modTime: time.Unix(1, 0),
		rev:     "rev-baseline",
	}
	src := object.NewStaticObjectInfo(o.remote, mtime, 7, true, nil, f)
	options := []fs.OpenOption{fs.MetadataOption{
		syncExpectedIDMetadataKey:  o.id,
		syncExpectedRevMetadataKey: o.rev,
		"unrelated":                "ignored",
	}}

	if err := o.Update(ctx, strings.NewReader("updated"), src, options...); err != nil {
		t.Fatal(err)
	}
	if beginBody["docid"] != "gns://personal/file" || beginBody["editedrev"] != "rev-baseline" {
		t.Fatalf("upload begin did not use expected baseline revision: %#v", beginBody)
	}
	if endBody["editedrev"] != "rev-baseline" {
		t.Fatalf("upload end did not use expected baseline revision: %#v", endBody)
	}
	if _, ok := beginBody[syncExpectedIDMetadataKey]; ok {
		t.Fatalf("sync metadata leaked into AnyShare upload body: %#v", beginBody)
	}
	if _, ok := beginBody[syncExpectedRevMetadataKey]; ok {
		t.Fatalf("sync metadata leaked into AnyShare upload body: %#v", beginBody)
	}
	if got := o.ID(); got != "gns://personal/file" {
		t.Fatalf("updated object ID = %q", got)
	}
	metadata, err := o.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := metadata[metadataRevisionKey]; got != "rev-new" {
		t.Fatalf("updated revision metadata = %q, want rev-new", got)
	}
}

func TestUpdateNormalizesServerRevisionConflict(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/efast/v1/file/osbeginupload" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    editRevisionConflictCode,
			"message": "edited revision is no longer current",
		})
	}))
	defer server.Close()

	f := &Fs{
		name: "pku",
		opt:  Options{Enc: defaultEncoding},
		api:  mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"}),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")

	o := &Object{
		fs:      f,
		remote:  "Personal/file.txt",
		id:      "gns://personal/file",
		size:    3,
		modTime: time.Unix(1, 0),
		rev:     "rev-baseline",
	}
	src := object.NewStaticObjectInfo(o.remote, time.Unix(2, 0), 7, true, nil, f)
	options := fs.MetadataOption{
		syncExpectedIDMetadataKey:  o.id,
		syncExpectedRevMetadataKey: o.rev,
	}

	err := o.Update(ctx, strings.NewReader("updated"), src, options)
	if err == nil {
		t.Fatal("expected server revision conflict")
	}
	if !fserrors.IsNoRetryError(err) {
		t.Fatalf("error = %v, want NoRetryError", err)
	}
	if !strings.Contains(err.Error(), "sync precondition failed") || !strings.Contains(err.Error(), "403203") {
		t.Fatalf("server revision conflict was not normalized: %v", err)
	}
}

func TestOpenChunkWriterRejectsStaleSyncPreconditionsBeforeMultipartIO(t *testing.T) {
	ctx := context.Background()
	unexpectedCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/file", "name": "file.bin", "size": 3,
				"rev": "rev-current", "client_mtime": int64(1_000_000),
			})
		default:
			unexpectedCalls++
			http.Error(w, `{"code":500001,"message":"unexpected call"}`, http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	f := &Fs{
		name:      "pku",
		opt:       Options{Enc: defaultEncoding},
		api:       mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"}),
		resumeDir: t.TempDir(),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")
	localRoot := t.TempDir()
	writeRepeatedTestFile(t, filepath.Join(localRoot, "source.bin"), 'x', 7, time.Unix(2, 0))
	src := newResumeLocalObject(t, ctx, localRoot, "source.bin")
	options := fs.MetadataOption{
		syncExpectedIDMetadataKey:  "gns://personal/file",
		syncExpectedRevMetadataKey: "rev-baseline",
	}

	_, _, err := f.OpenChunkWriter(ctx, "Personal/file.bin", src, options)
	if err == nil {
		t.Fatal("expected stale multipart precondition error")
	}
	if !fserrors.IsNoRetryError(err) || !strings.Contains(err.Error(), "sync precondition failed") {
		t.Fatalf("unexpected stale multipart error: %v", err)
	}
	if unexpectedCalls != 0 {
		t.Fatalf("stale multipart precondition performed %d upload API calls", unexpectedCalls)
	}
}

func TestOpenChunkWriterUsesExpectedRevisionForMultipartInit(t *testing.T) {
	ctx := context.Background()
	var initBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/file", "name": "file.bin", "size": 3,
				"rev": "rev-baseline", "client_mtime": int64(1_000_000),
			})
		case "/api/efast/v1/file/osoption":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"partminsize": 1, "partmaxsize": 64, "partmaxnum": 100,
			})
		case "/api/efast/v1/file/osinitmultiupload":
			_ = json.NewDecoder(r.Body).Decode(&initBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/file", "rev": "upload-rev", "uploadid": "upload-id",
			})
		case "/api/efast/v1/file/osuploadpart":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authrequests": map[string][]string{"1": {"PUT", "https://object.invalid/part"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{
		name:      "pku",
		opt:       Options{Enc: defaultEncoding},
		api:       mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"}),
		resumeDir: t.TempDir(),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")
	localRoot := t.TempDir()
	writeRepeatedTestFile(t, filepath.Join(localRoot, "source.bin"), 'x', 7, time.Unix(2, 0))
	src := newResumeLocalObject(t, ctx, localRoot, "source.bin")
	options := fs.MetadataOption{
		syncExpectedIDMetadataKey:  "gns://personal/file",
		syncExpectedRevMetadataKey: "rev-baseline",
	}

	_, writer, err := f.OpenChunkWriter(ctx, "Personal/file.bin", src, options)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Abort(ctx)
	if initBody["docid"] != "gns://personal/file" || initBody["editedrev"] != "rev-baseline" {
		t.Fatalf("multipart init did not use expected baseline revision: %#v", initBody)
	}
	if _, ok := initBody[syncExpectedIDMetadataKey]; ok {
		t.Fatalf("sync ID metadata leaked into multipart init: %#v", initBody)
	}
	if _, ok := initBody[syncExpectedRevMetadataKey]; ok {
		t.Fatalf("sync revision metadata leaked into multipart init: %#v", initBody)
	}
}

func TestSyncDeleteCommandChecksRevisionBeforeExactIDDelete(t *testing.T) {
	ctx := context.Background()
	currentRev := "rev-current"
	deleteCalls := 0
	var deletedID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/metadata":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": body["docid"], "file_name": "file.txt", "size": 7, "rev": currentRev,
			})
		case "/api/efast/v1/file/delete":
			deleteCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			deletedID, _ = body["docid"].(string)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{api: mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"})}
	id := "gns://personal/file"
	_, err := f.Command(ctx, "sync-delete", []string{id}, map[string]string{"expected-rev": "rev-stale"})
	if err == nil || !fserrors.IsNoRetryError(err) {
		t.Fatalf("stale sync-delete error = %v, want NoRetryError", err)
	}
	if deleteCalls != 0 {
		t.Fatalf("stale sync-delete performed %d delete calls", deleteCalls)
	}

	result, err := f.Command(ctx, "sync-delete", []string{id}, map[string]string{"expected-rev": currentRev})
	if err != nil {
		t.Fatal(err)
	}
	if deleteCalls != 1 || deletedID != id {
		t.Fatalf("sync-delete calls=%d id=%q, want one delete of %q", deleteCalls, deletedID, id)
	}
	got, ok := result.(map[string]any)
	if !ok || got["deleted"] != true || got["id"] != id {
		t.Fatalf("sync-delete result = %#v", result)
	}
}

func TestSyncMoveCommandUsesExactID(t *testing.T) {
	ctx := context.Background()
	var renameBody map[string]any
	var moveBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/rename":
			_ = json.NewDecoder(r.Body).Decode(&renameBody)
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/move":
			_ = json.NewDecoder(r.Body).Decode(&moveBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": "gns://personal/sub/file"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{
		opt: Options{Enc: defaultEncoding},
		api: mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"}),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")
	f.dirCache.Put("Personal/Sub", "gns://personal/sub")
	id := "gns://personal/file"

	result, err := f.Command(ctx, "sync-move", []string{id, "Personal/renamed.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if renameBody["docid"] != id || renameBody["name"] != "renamed.txt" || renameBody["ondup"] != float64(1) {
		t.Fatalf("same-parent sync-move body = %#v", renameBody)
	}
	if got := result.(map[string]any)["id"]; got != id {
		t.Fatalf("same-parent sync-move returned id %v, want %q", got, id)
	}

	result, err = f.Command(ctx, "sync-move", []string{id, "Personal/Sub/moved.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if moveBody["docid"] != id || moveBody["destparent"] != "gns://personal/sub" || moveBody["new_name"] != "moved.txt" || moveBody["ondup"] != float64(1) {
		t.Fatalf("cross-parent sync-move body = %#v", moveBody)
	}
	if got := result.(map[string]any)["id"]; got != "gns://personal/sub/file" {
		t.Fatalf("cross-parent sync-move returned id %v", got)
	}
}

func TestSyncMoveCommandDoesNotCreateMissingDestinationParent(t *testing.T) {
	ctx := context.Background()
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
		case "/api/efast/v1/file/rename", "/api/efast/v1/file/move", "/api/efast/v1/dir/create":
			mutations++
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{
		opt: Options{Enc: defaultEncoding},
		api: mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"}),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")
	_, err := f.Command(ctx, "sync-move", []string{"gns://personal/file", "Personal/Missing/moved.txt"}, nil)
	if err == nil {
		t.Fatal("sync-move unexpectedly accepted a missing destination parent")
	}
	if mutations != 0 {
		t.Fatalf("sync-move performed %d mutations while resolving a missing destination parent", mutations)
	}
}
