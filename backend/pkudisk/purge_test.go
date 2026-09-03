package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rclone/rclone/fs/config/configmap"
)

func TestPurgeUsesNativeRecursiveDirectoryDelete(t *testing.T) {
	var deleteCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "gns://personal", "name": "Personal", "type": "user_doc_lib",
			}})
		case "/api/efast/v1/dir/list":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal" {
				http.Error(w, "bad docid", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dirs":  []map[string]any{{"docid": "gns://personal/docs", "name": "docs", "size": -1}},
				"files": []map[string]any{},
			})
		case "/api/efast/v1/dir/delete":
			deleteCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal/docs" || body["check_upload_process"] != true {
				t.Fatalf("delete body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	backend, err := NewFs(context.Background(), "pku", "Personal/docs", configmap.Simple{
		"auth":         "token",
		"access_token": "test-token",
		"base_url":     server.URL,
		"encoding":     defaultEncoding.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.(*Fs).Purge(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", deleteCalls)
	}
}

func TestPurgeProtectsDocumentLibrary(t *testing.T) {
	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "gns://personal", "name": "Personal", "type": "user_doc_lib",
			}})
		case "/api/efast/v1/dir/delete":
			deleteCalled = true
			http.Error(w, "must not delete library", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	backend, err := NewFs(context.Background(), "pku", "Personal", configmap.Simple{
		"auth":         "token",
		"access_token": "test-token",
		"base_url":     server.URL,
		"encoding":     defaultEncoding.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.(*Fs).Purge(context.Background(), ""); err == nil {
		t.Fatal("purging a document library unexpectedly succeeded")
	}
	if deleteCalled {
		t.Fatal("document library purge reached dir/delete")
	}
}
