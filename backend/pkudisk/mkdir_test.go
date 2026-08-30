package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rclone/rclone/fs/config/configmap"
)

func TestMkdirCreatesRootWhenFsIsRootedAtMissingDirectory(t *testing.T) {
	ctx := context.Background()
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "gns://personal", "name": "Personal"}})
		case "/api/efast/v1/dir/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
		case "/api/efast/v1/dir/create":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != "gns://personal" || body["name"] != "new" {
				t.Fatalf("unexpected create body: %#v", body)
			}
			created = true
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": "gns://personal/new", "name": "new"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	backend, err := NewFs(ctx, "pku", "Personal/new", configmap.Simple{
		"auth":         "token",
		"access_token": "test-token",
		"base_url":     server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Mkdir(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("root-relative Mkdir did not create the missing backend root")
	}
}
