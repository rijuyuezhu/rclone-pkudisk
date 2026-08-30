package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
)

func TestListVirtualRootAndDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "gns://personal", "name": "Personal", "type": "user_doc_lib", "modified_at": "2026-08-30T00:00:00Z",
			}})
		case "/api/efast/v1/dir/list":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal" {
				http.Error(w, "bad docid", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dirs":  []map[string]any{{"docid": "gns://personal/dir", "name": "docs", "size": -1, "modified": int64(1_788_000_000_000_000)}},
				"files": []map[string]any{{"docid": "gns://personal/file", "name": "hello.txt", "size": 5, "modified": int64(1_788_000_001_000_000), "rev": "rev-1"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	m := configmap.Simple{
		"auth":         "token",
		"access_token": "test-token",
		"base_url":     server.URL,
	}
	backend, err := NewFs(context.Background(), "pku", "", m)
	if err != nil {
		t.Fatal(err)
	}
	f := backend.(*Fs)
	root, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(root) != 1 || root[0].Remote() != "Personal" {
		t.Fatalf("root listing = %#v", root)
	}
	entries, err := f.List(context.Background(), "Personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(entries))
	}
	var file fs.Object
	for _, item := range entries {
		if object, ok := item.(fs.Object); ok {
			file = object
		}
	}
	if file == nil || file.Remote() != "Personal/hello.txt" || file.Size() != 5 {
		t.Fatalf("file entry = %#v", file)
	}
	want := time.Unix(0, 1_788_000_001_000_000*int64(time.Microsecond)).UTC()
	if !file.ModTime(context.Background()).Equal(want) {
		t.Fatalf("modtime = %s, want %s", file.ModTime(context.Background()), want)
	}
}
