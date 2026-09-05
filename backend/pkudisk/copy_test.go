package pkudisk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rclone/rclone/fs"
)

func writePathNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    getInfoPathNotFoundCode,
		"message": "not found",
	})
}

func TestCopySameNameNewUsesFailOnDuplicate(t *testing.T) {
	ctx := context.Background()
	const copiedID = "gns://personal/dst/copied"
	copied := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			if !copied {
				writePathNotFound(w)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": copiedID, "name": "file.txt", "size": 7, "rev": "copy-rev",
			})
		case "/api/efast/v1/file/copy":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal/src/source" || body["destparent"] != "gns://personal/dst" || body["ondup"] != float64(1) {
				t.Fatalf("unexpected copy body: %#v", body)
			}
			copied = true
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": copiedID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	source := &Object{fs: f, remote: "Personal/src/file.txt", id: "gns://personal/src/source", size: 7, rev: "src-rev"}
	got, err := f.Copy(ctx, source, "Personal/dst/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.(*Object).id != copiedID || !copied {
		t.Fatalf("copied object = %#v copied=%v", got, copied)
	}
}

func TestCopyExistingDestinationFallsBackWithoutMutation(t *testing.T) {
	ctx := context.Background()
	mutated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"docid": "gns://personal/dst/old", "name": "file.txt", "size": 7, "rev": "dst-rev",
			})
		case "/api/efast/v1/file/copy", "/api/efast/v1/file/rename", "/api/efast/v1/file/delete":
			mutated = true
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	source := &Object{fs: f, remote: "Personal/src/file.txt", id: "gns://personal/src/source", size: 7}
	_, err := f.Copy(ctx, source, "Personal/dst/file.txt")
	if !errors.Is(err, fs.ErrorCantCopy) {
		t.Fatalf("error = %v, want ErrorCantCopy", err)
	}
	if mutated {
		t.Fatal("copy mutated an existing destination without a revision guard")
	}
}

func TestCopyDifferentNameUsesReturnedIDForRename(t *testing.T) {
	ctx := context.Background()
	const copiedID = "gns://personal/dst/server-generated-copy"
	copied := false
	renamed := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			if !renamed {
				writePathNotFound(w)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": copiedID, "name": "final.txt", "size": 9, "rev": "copy-rev"})
		case "/api/efast/v1/file/copy":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["ondup"] != float64(2) {
				t.Fatalf("copy ondup = %#v, want 2", body["ondup"])
			}
			copied = true
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": copiedID, "name": "source (1).txt"})
		case "/api/efast/v1/file/rename":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !copied || body["docid"] != copiedID || body["name"] != "final.txt" || body["ondup"] != float64(1) {
				t.Fatalf("unexpected rename body: %#v", body)
			}
			renamed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	source := &Object{fs: f, remote: "Personal/src/source.txt", id: "gns://personal/src/source", size: 9}
	got, err := f.Copy(ctx, source, "Personal/dst/final.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !copied || !renamed || got.(*Object).id != copiedID {
		t.Fatalf("copy=%v rename=%v object=%#v", copied, renamed, got)
	}
}

func TestCopyRenameFailureDeletesOnlyNewCopy(t *testing.T) {
	ctx := context.Background()
	const copiedID = "gns://personal/dst/new-copy"
	deletedID := ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			writePathNotFound(w)
		case "/api/efast/v1/file/copy":
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": copiedID})
		case "/api/efast/v1/file/rename":
			http.Error(w, `{"code":500001,"message":"rename failed"}`, http.StatusInternalServerError)
		case "/api/efast/v1/file/delete":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			deletedID, _ = body["docid"].(string)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	sourceID := "gns://personal/src/source"
	source := &Object{fs: f, remote: "Personal/src/source.txt", id: sourceID, size: 5}
	_, err := f.Copy(ctx, source, "Personal/dst/final.txt")
	if err == nil {
		t.Fatal("expected rename failure")
	}
	if deletedID != copiedID {
		t.Fatalf("deleted docid = %q, want copied id %q (source=%q)", deletedID, copiedID, sourceID)
	}
}
