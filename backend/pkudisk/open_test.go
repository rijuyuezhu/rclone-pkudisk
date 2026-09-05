package pkudisk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
)

func newOpenTestObject(t *testing.T, objectHandler http.HandlerFunc) (*Object, func()) {
	t.Helper()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osdownload":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{
				"authrequest": []any{nil, server.URL + "/object"},
			}); err != nil {
				t.Errorf("encode osdownload response: %v", err)
			}
		case "/object":
			objectHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))

	client := &apiClient{
		baseURL:    server.URL,
		http:       server.Client(),
		objectHTTP: server.Client(),
		tokens:     &staticTokenProvider{token: "test-token"},
	}
	obj := &Object{
		fs:     &Fs{api: client},
		remote: "file.bin",
		id:     "gns://personal/file-id",
		size:   8,
		rev:    "rev-1",
	}
	return obj, server.Close
}

func TestOpenRejectsIgnoredRange(t *testing.T) {
	obj, closeServer := newOpenTestObject(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=4-7" {
			t.Errorf("Range header = %q, want bytes=4-7", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "abcdefgh")
	})
	defer closeServer()

	r, err := obj.Open(context.Background(), &fs.RangeOption{Start: 4, End: 7})
	if err == nil {
		r.Close()
		t.Fatal("Open accepted a 200 response that ignored the Range request")
	}
	if !errors.Is(err, fs.ErrorRangeIgnored) {
		t.Fatalf("Open error = %v, want fs.ErrorRangeIgnored", err)
	}
	if !strings.Contains(err.Error(), "range") {
		t.Fatalf("Open error = %v, want range validation error", err)
	}
}

func TestOpenAcceptsMatchingRange(t *testing.T) {
	obj, closeServer := newOpenTestObject(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=4-7" {
			t.Errorf("Range header = %q, want bytes=4-7", got)
		}
		w.Header().Set("Content-Range", "bytes 4-7/8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "efgh")
	})
	defer closeServer()

	r, err := obj.Open(context.Background(), &fs.RangeOption{Start: 4, End: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "efgh" {
		t.Fatalf("range result = %q, want efgh", got)
	}
}

func TestOpenAcceptsWholeObjectRangeWith200(t *testing.T) {
	obj, closeServer := newOpenTestObject(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-7" {
			t.Errorf("Range header = %q, want bytes=0-7", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "abcdefgh")
	})
	defer closeServer()

	r, err := obj.Open(context.Background(), &fs.RangeOption{Start: 0, End: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("whole-range result = %q, want abcdefgh", got)
	}
}

func TestOpenRejectsMismatchedContentRange(t *testing.T) {
	obj, closeServer := newOpenTestObject(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abcd")
	})
	defer closeServer()

	r, err := obj.Open(context.Background(), &fs.RangeOption{Start: 4, End: 7})
	if err == nil {
		r.Close()
		t.Fatal("Open accepted a mismatched Content-Range")
	}
	if !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("Open error = %v, want Content-Range validation error", err)
	}
}

func TestOpenAcceptsFullResponseWithoutRange(t *testing.T) {
	obj, closeServer := newOpenTestObject(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "" {
			t.Errorf("unexpected Range header %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "abcdefgh")
	})
	defer closeServer()

	r, err := obj.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("full result = %q, want abcdefgh", got)
	}
}
