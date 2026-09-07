package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rclone/rclone/fs/fserrors"
)

func TestSyncDeleteDirCommandRequiresExactEmptyDirectory(t *testing.T) {
	ctx := context.Background()
	id := "gns://personal/empty-dir"
	nonEmpty := true
	listCalls := 0
	deleteCalls := 0
	var listedID, deletedID string
	var deleteCheckUploadProcess any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/dir/list":
			listCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			listedID, _ = body["docid"].(string)
			if nonEmpty {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"dirs":  []map[string]any{{"docid": id + "/child", "name": "child"}},
					"files": []any{},
				})
				return
			}
			_, _ = w.Write([]byte(`{"dirs":[],"files":[]}`))
		case "/api/efast/v1/dir/delete":
			deleteCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			deletedID, _ = body["docid"].(string)
			deleteCheckUploadProcess = body["check_upload_process"]
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := &Fs{api: mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"})}
	_, err := f.Command(ctx, "sync-delete-dir", []string{id}, nil)
	if err == nil || !fserrors.IsNoRetryError(err) {
		t.Fatalf("non-empty sync-delete-dir error = %v, want NoRetryError", err)
	}
	if listCalls != 1 || listedID != id || deleteCalls != 0 {
		t.Fatalf("non-empty call sequence: listCalls=%d listedID=%q deleteCalls=%d", listCalls, listedID, deleteCalls)
	}

	nonEmpty = false
	result, err := f.Command(ctx, "sync-delete-dir", []string{id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 2 || listedID != id || deleteCalls != 1 || deletedID != id {
		t.Fatalf("empty call sequence: listCalls=%d listedID=%q deleteCalls=%d deletedID=%q", listCalls, listedID, deleteCalls, deletedID)
	}
	if deleteCheckUploadProcess != true {
		t.Fatalf("directory delete check_upload_process = %#v, want true", deleteCheckUploadProcess)
	}
	got, ok := result.(map[string]any)
	if !ok || got["deleted"] != true || got["id"] != id {
		t.Fatalf("sync-delete-dir result = %#v", result)
	}
}

func TestSyncDeleteDirCommandRefusesDocumentLibraryID(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer server.Close()

	f := &Fs{api: mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"})}
	if _, err := f.Command(ctx, "sync-delete-dir", []string{"gns://personal"}, nil); err == nil {
		t.Fatal("sync-delete-dir accepted a document-library ID")
	}
	if calls != 0 {
		t.Fatalf("document-library rejection made %d API calls", calls)
	}
}
