package pkudisk

import (
	"context"
	"encoding/json"
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
