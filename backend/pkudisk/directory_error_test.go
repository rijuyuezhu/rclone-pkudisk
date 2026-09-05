package pkudisk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
)

func TestDirectoryLookupAPIErrorsArePreserved(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			http.Error(w, `{"message":"point lookup unavailable"}`, http.StatusInternalServerError)
		case "/api/efast/v1/dir/list":
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	checks := []struct {
		name     string
		notFound error
		call     func() error
	}{
		{
			name:     "List",
			notFound: fs.ErrorDirNotFound,
			call: func() error {
				_, err := f.List(ctx, "Personal/broken")
				return err
			},
		},
		{
			name:     "NewObject",
			notFound: fs.ErrorObjectNotFound,
			call: func() error {
				_, err := f.NewObject(ctx, "Personal/broken/file.txt")
				return err
			},
		},
		{name: "Rmdir", notFound: fs.ErrorDirNotFound, call: func() error { return f.Rmdir(ctx, "Personal/broken") }},
		{name: "Purge", notFound: fs.ErrorDirNotFound, call: func() error { return f.Purge(ctx, "Personal/broken") }},
	}

	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.call()
			if err == nil {
				t.Fatal("expected API lookup error")
			}
			if errors.Is(err, check.notFound) {
				t.Fatalf("API error was collapsed into %v: %v", check.notFound, err)
			}
			if !strings.Contains(err.Error(), "403") {
				t.Fatalf("error did not preserve upstream failure: %v", err)
			}
		})
	}
}

func TestDirectoryLookupMissingStillMapsToRcloneNotFound(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/efast/v1/file/getinfobypath":
			writePathNotFound(w)
		case "/api/efast/v1/dir/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := newCachedTestFs(t, ctx, server.URL)
	if _, err := f.List(ctx, "Personal/missing"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("List error = %v, want ErrorDirNotFound", err)
	}
	if _, err := f.NewObject(ctx, "Personal/missing/file.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject error = %v, want ErrorObjectNotFound", err)
	}
	if err := f.Rmdir(ctx, "Personal/missing"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("Rmdir error = %v, want ErrorDirNotFound", err)
	}
	if err := f.Purge(ctx, "Personal/missing"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("Purge error = %v, want ErrorDirNotFound", err)
	}
}
