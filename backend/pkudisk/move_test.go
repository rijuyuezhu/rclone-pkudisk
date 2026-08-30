package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/dircache"
)

func newCachedTestFs(t *testing.T, ctx context.Context, serverURL string) *Fs {
	t.Helper()
	f := &Fs{
		name: "pku",
		api:  newAPIClient(serverURL, &staticTokenProvider{token: "test-token"}),
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

func TestMoveResolvesDocIDAfterParentChangeBeforeRename(t *testing.T) {
	ctx := context.Background()
	const (
		oldID = "gns://personal/src/old-file-id"
		newID = "gns://personal/dst/new-file-id"
	)
	moved := false
	renamed := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/move":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != oldID || body["destparent"] != "gns://personal/dst" {
				t.Fatalf("unexpected move body: %#v", body)
			}
			moved = true
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/rename":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != newID {
				t.Fatalf("rename used stale docid: %#v", body)
			}
			if body["name"] != "new.txt" {
				t.Fatalf("unexpected rename body: %#v", body)
			}
			renamed = true
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/dir/list":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal/dst" {
				t.Fatalf("unexpected list body: %#v", body)
			}
			files := []map[string]any{}
			if moved {
				name := "old.txt"
				if renamed {
					name = "new.txt"
				}
				files = append(files, map[string]any{
					"docid": newID,
					"name":  name,
					"size":  7,
					"rev":   "rev-1",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": files})
		default:
			http.NotFound(w, r)
		}
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
	if !moved || !renamed {
		t.Fatalf("move=%v rename=%v", moved, renamed)
	}
}

func TestDirMoveResolvesDocIDAfterParentChangeBeforeRename(t *testing.T) {
	ctx := context.Background()
	const (
		oldID = "gns://personal/src/old-dir-id"
		newID = "gns://personal/dst/new-dir-id"
	)
	moved := false
	renamed := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/move":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != oldID || body["destparent"] != "gns://personal/dst" {
				t.Fatalf("unexpected move body: %#v", body)
			}
			moved = true
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/dir/rename":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != newID {
				t.Fatalf("rename used stale docid: %#v", body)
			}
			if body["name"] != "newdir" {
				t.Fatalf("unexpected rename body: %#v", body)
			}
			renamed = true
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/dir/list":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal/dst" {
				t.Fatalf("unexpected list body: %#v", body)
			}
			dirs := []map[string]any{}
			if moved {
				name := "olddir"
				if renamed {
					name = "newdir"
				}
				dirs = append(dirs, map[string]any{"docid": newID, "name": name, "size": -1})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": dirs, "files": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	f.dirCache.Put("Personal/src/olddir", oldID)
	if err := f.DirMove(ctx, f, "Personal/src/olddir", "Personal/dst/newdir"); err != nil {
		t.Fatal(err)
	}
	if !moved || !renamed {
		t.Fatalf("move=%v rename=%v", moved, renamed)
	}
}

func TestRelocateEntryRenamesBeforeMoveWhenDestinationHasSourceName(t *testing.T) {
	ctx := context.Background()
	const (
		oldID      = "gns://personal/src/source-id"
		newID      = "gns://personal/dst/new-id"
		conflictID = "gns://personal/dst/conflict-id"
	)
	renamed := false
	moved := false
	var operations []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/list":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body["docid"] {
			case "gns://personal/src":
				name := "source.txt"
				if renamed {
					name = "final.txt"
				}
				files := []map[string]any{}
				if !moved {
					files = append(files, map[string]any{"docid": oldID, "name": name, "size": 1})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": files})
			case "gns://personal/dst":
				files := []map[string]any{{"docid": conflictID, "name": "source.txt", "size": 1}}
				if moved {
					files = append(files, map[string]any{"docid": newID, "name": "final.txt", "size": 1})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": files})
			default:
				t.Fatalf("unexpected list parent: %#v", body)
			}
		case "/api/efast/v1/file/rename":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != oldID || body["name"] != "final.txt" {
				t.Fatalf("unexpected rename body: %#v", body)
			}
			operations = append(operations, "rename")
			renamed = true
			_, _ = w.Write([]byte(`{}`))
		case "/api/efast/v1/file/move":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !renamed || body["docid"] != oldID || body["destparent"] != "gns://personal/dst" {
				t.Fatalf("unexpected move body: %#v", body)
			}
			operations = append(operations, "move")
			moved = true
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{opt: Options{Enc: defaultEncoding}, api: newAPIClient(server.URL, &staticTokenProvider{token: "test-token"})}
	gotID, err := f.relocateEntry(ctx, oldID, "gns://personal/src", "gns://personal/dst", "source.txt", "final.txt", false, fs.ErrorCantMove)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != newID {
		t.Fatalf("relocated ID = %q, want %q", gotID, newID)
	}
	if len(operations) != 2 || operations[0] != "rename" || operations[1] != "move" {
		t.Fatalf("operation order = %v, want [rename move]", operations)
	}
}

func TestRelocateEntryFallsBackWhenBothIntermediateNamesConflict(t *testing.T) {
	ctx := context.Background()
	mutated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/list":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			files := []map[string]any{}
			switch body["docid"] {
			case "gns://personal/src":
				files = append(files,
					map[string]any{"docid": "source-id", "name": "source.txt", "size": 1},
					map[string]any{"docid": "final-id", "name": "final.txt", "size": 1},
				)
			case "gns://personal/dst":
				files = append(files, map[string]any{"docid": "conflict-id", "name": "source.txt", "size": 1})
			default:
				t.Fatalf("unexpected list parent: %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": files})
		case "/api/efast/v1/file/rename", "/api/efast/v1/file/move":
			mutated = true
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{opt: Options{Enc: defaultEncoding}, api: newAPIClient(server.URL, &staticTokenProvider{token: "test-token"})}
	_, err := f.relocateEntry(ctx, "source-id", "gns://personal/src", "gns://personal/dst", "source.txt", "final.txt", false, fs.ErrorCantMove)
	if err != fs.ErrorCantMove {
		t.Fatalf("error = %v, want ErrorCantMove", err)
	}
	if mutated {
		t.Fatal("relocateEntry mutated remote despite both intermediate names conflicting")
	}
}
