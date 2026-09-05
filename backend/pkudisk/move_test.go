package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rclone/rclone/lib/dircache"
)

func newCachedTestFs(t *testing.T, ctx context.Context, serverURL string) *Fs {
	t.Helper()
	f := &Fs{
		name: "pku",
		api:  mustNewAPIClient(t, context.Background(), serverURL, &staticTokenProvider{token: "test-token"}),
	}
	f.dirCache = dircache.New("", virtualRootID, f)
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("Personal", "gns://personal")
	f.dirCache.Put("Personal/src", "gns://personal/src")
	f.dirCache.Put("Personal/dst", "gns://personal/dst")
	return f
}

func TestMoveUsesAtomicMoveAndRename(t *testing.T) {
	ctx := context.Background()
	const (
		oldID = "gns://personal/src/old-file-id"
		newID = "gns://personal/dst/new-file-id"
	)
	moveCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/efast/v1/file/move" {
			t.Fatalf("unexpected request after atomic move: %s", r.URL.Path)
		}
		moveCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["docid"] != oldID || body["destparent"] != "gns://personal/dst" || body["new_name"] != "new.txt" || body["ondup"] != float64(1) {
			t.Fatalf("unexpected move body: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"docid": newID})
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	source := &Object{fs: f, remote: "Personal/src/old.txt", id: oldID, size: 7, rev: "rev-1"}
	got, err := f.Move(ctx, source, "Personal/dst/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	object := got.(*Object)
	if object.id != newID || object.remote != "Personal/dst/new.txt" {
		t.Fatalf("moved object = %#v", object)
	}
	if moveCalls != 1 {
		t.Fatalf("move calls = %d, want 1", moveCalls)
	}
}

func TestDirMoveUsesAtomicMoveAndRename(t *testing.T) {
	ctx := context.Background()
	const (
		oldID = "gns://personal/src/old-dir-id"
		newID = "gns://personal/dst/new-dir-id"
	)
	moveCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/list":
			if moveCalls != 0 {
				t.Fatalf("unexpected directory lookup after atomic move")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
		case "/api/efast/v1/dir/move":
			moveCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != oldID || body["destparent"] != "gns://personal/dst" || body["new_name"] != "newdir" || body["ondup"] != float64(1) || body["check_upload_process"] != true {
				t.Fatalf("unexpected move body: %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": newID})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	f.dirCache.Put("Personal/src/olddir", oldID)
	if err := f.DirMove(ctx, f, "Personal/src/olddir", "Personal/dst/newdir"); err != nil {
		t.Fatal(err)
	}
	if moveCalls != 1 {
		t.Fatalf("move calls = %d, want 1", moveCalls)
	}
}

func TestRelocateEntrySameParentRenamePreservesDocID(t *testing.T) {
	ctx := context.Background()
	const id = "gns://personal/src/file-id"
	renameCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/efast/v1/file/rename" {
			t.Fatalf("unexpected request after rename: %s", r.URL.Path)
		}
		renameCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["docid"] != id || body["name"] != "new.txt" || body["ondup"] != float64(1) {
			t.Fatalf("unexpected rename body: %#v", body)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	f := &Fs{opt: Options{Enc: defaultEncoding}, api: mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})}
	gotID, err := f.relocateEntry(ctx, id, "gns://personal/src", "gns://personal/src", "old.txt", "new.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id {
		t.Fatalf("renamed ID = %q, want %q", gotID, id)
	}
	if renameCalls != 1 {
		t.Fatalf("rename calls = %d, want 1", renameCalls)
	}
}

func TestRelocateEntrySameNameMoveOmitsNewName(t *testing.T) {
	ctx := context.Background()
	const newID = "gns://personal/dst/file-id"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["new_name"]; ok {
			t.Fatalf("same-name move unexpectedly sent new_name: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"docid": newID})
	}))
	defer server.Close()

	f := &Fs{opt: Options{Enc: defaultEncoding}, api: mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})}
	gotID, err := f.relocateEntry(ctx, "old-id", "gns://personal/src", "gns://personal/dst", "same.txt", "same.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != newID {
		t.Fatalf("moved ID = %q, want %q", gotID, newID)
	}
}

func TestRelocateEntryMissingMoveDocIDReportsAmbiguousState(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	f := &Fs{opt: Options{Enc: defaultEncoding}, api: mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})}
	_, err := f.relocateEntry(ctx, "old-id", "gns://personal/src", "gns://personal/dst", "old.txt", "new.txt", false)
	if err == nil || !strings.Contains(err.Error(), "missing docid") || !strings.Contains(err.Error(), "remote state may have changed") {
		t.Fatalf("error = %v, want explicit ambiguous-state error", err)
	}
}
