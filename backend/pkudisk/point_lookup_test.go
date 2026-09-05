package pkudisk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/lib/dircache"
)

type noTraversalDirCacher struct {
	findCalls int
}

func (c *noTraversalDirCacher) FindLeaf(context.Context, string, string) (string, bool, error) {
	c.findCalls++
	return "", false, errors.New("unexpected traversal")
}

func (*noTraversalDirCacher) CreateDir(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected create")
}

func TestEncodedNamePathPreservesLibraryName(t *testing.T) {
	f := &Fs{root: "Library:Raw/parent:name", opt: Options{Enc: defaultEncoding}}
	got := f.encodedNamePath("child?.txt")
	want := "Library:Raw/" + f.encodeName("parent:name") + "/" + f.encodeName("child?.txt")
	if got != want {
		t.Fatalf("encoded name path = %q, want %q", got, want)
	}
}

func TestSeedDirCacheFromDocIDAvoidsTraversal(t *testing.T) {
	cacher := &noTraversalDirCacher{}
	dc := dircache.New("Library/one/two", virtualRootID, cacher)
	if !seedDirCacheFromDocID(dc, "Library/one/two", "gns://LIB/ONE/TWO") {
		t.Fatal("failed to seed a valid GNS path")
	}
	if err := dc.FindRoot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	rootID, err := dc.RootID(context.Background(), false)
	if err != nil || rootID != "gns://LIB/ONE/TWO" {
		t.Fatalf("root id = %q err=%v", rootID, err)
	}
	parentID, err := dc.RootParentID(context.Background(), false)
	if err != nil || parentID != "gns://LIB/ONE" {
		t.Fatalf("parent id = %q err=%v", parentID, err)
	}
	if cacher.findCalls != 0 {
		t.Fatalf("FindLeaf calls = %d, want 0", cacher.findCalls)
	}
}

func TestNewObjectUsesGetInfoByPath(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/efast/v1/file/getinfobypath" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["namepath"] != "Library/dir/file.txt" {
			t.Fatalf("namepath = %#v", body["namepath"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"docid":        "gns://LIB/DIR/FILE",
			"name":         "file.txt",
			"size":         123,
			"modified":     int64(1_788_000_000_000_000),
			"client_mtime": int64(1_577_934_245_123_456),
			"rev":          "rev-1",
		})
	}))
	defer server.Close()

	f := &Fs{
		root: "Library/dir",
		opt:  Options{Enc: defaultEncoding},
		api:  newAPIClient(server.URL, &staticTokenProvider{token: "test-token"}),
	}
	object, err := f.NewObject(context.Background(), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if object.Size() != 123 {
		t.Fatalf("size = %d", object.Size())
	}
	wantTime := parseModifiedMicros(1_577_934_245_123_456)
	if !object.ModTime(context.Background()).Equal(wantTime) {
		t.Fatalf("modtime = %s, want %s", object.ModTime(context.Background()), wantTime)
	}
	if object.(*Object).rev != "rev-1" {
		t.Fatalf("rev = %q", object.(*Object).rev)
	}
}

func TestNewObjectMapsGetInfoNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/efast/v1/file/getinfobypath" {
			t.Fatalf("unexpected fallback request %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": getInfoPathNotFoundCode, "message": "not found"})
	}))
	defer server.Close()

	f := &Fs{
		root: "Library/dir",
		opt:  Options{Enc: defaultEncoding},
		api:  newAPIClient(server.URL, &staticTokenProvider{token: "test-token"}),
	}
	_, err := f.NewObject(context.Background(), "missing.txt")
	if !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("error = %v, want ErrorObjectNotFound", err)
	}
}

func TestNewFsSeedsDirectoryRootFromGetInfo(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/efast/v1/file/getinfobypath" {
			t.Fatalf("unexpected traversal request %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"docid": "gns://LIB/ONE/TWO", "name": "two", "size": -1, "modified": int64(1),
		})
	}))
	defer server.Close()

	backend, err := NewFs(context.Background(), "pku", "Library/one/two", configmap.Simple{
		"auth": "token", "access_token": "test-token", "base_url": server.URL, "encoding": defaultEncoding.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := backend.(*Fs)
	rootID, err := f.dirCache.RootID(context.Background(), false)
	if err != nil || rootID != "gns://LIB/ONE/TWO" {
		t.Fatalf("root id = %q err=%v", rootID, err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestNewFsDetectsFileRootFromGetInfo(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/efast/v1/file/getinfobypath" {
			t.Fatalf("unexpected traversal request %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"docid": "gns://LIB/ONE/FILE", "name": "file.txt", "size": 10,
			"modified": time.Now().UnixMicro(), "client_mtime": time.Now().UnixMicro(), "rev": "rev-1",
		})
	}))
	defer server.Close()

	backend, err := NewFs(context.Background(), "pku", "Library/one/file.txt", configmap.Simple{
		"auth": "token", "access_token": "test-token", "base_url": server.URL, "encoding": defaultEncoding.String(),
	})
	if !errors.Is(err, fs.ErrorIsFile) {
		t.Fatalf("NewFs error = %v, want ErrorIsFile", err)
	}
	f := backend.(*Fs)
	if f.root != "Library/one" {
		t.Fatalf("root = %q, want parent", f.root)
	}
	rootID, rootErr := f.dirCache.RootID(context.Background(), false)
	if rootErr != nil || rootID != "gns://LIB/ONE" {
		t.Fatalf("root id = %q err=%v", rootID, rootErr)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}
