package pkudisk

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
)

type rotatingTokenProvider struct {
	refreshCalls []bool
}

func (p *rotatingTokenProvider) Token(_ context.Context, refresh bool) (string, error) {
	p.refreshCalls = append(p.refreshCalls, refresh)
	if refresh {
		return "fresh-token", nil
	}
	return "stale-token", nil
}

func TestAPIClientRetriesObservedAuthSessionCodes(t *testing.T) {
	provider := &rotatingTokenProvider{}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer stale-token":
			// AnyShare can report auth/session failures as a JSON error on HTTP 200.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    401001033,
				"message": "client restricted",
			})
		case "Bearer fresh-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.Error(w, "unexpected token", http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	client := mustNewAPIClient(t, context.Background(), server.URL, provider)
	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.doJSON(context.Background(), http.MethodGet, "test", nil, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatal("retry did not return successful response")
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(provider.refreshCalls) != 2 || provider.refreshCalls[0] || !provider.refreshCalls[1] {
		t.Fatalf("refresh calls = %#v, want [false true]", provider.refreshCalls)
	}
}

func TestListDirRetriesTransientServerError(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, `{"message":"temporary"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
	}))
	defer server.Close()

	client := mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})
	if _, err := client.listDir(context.Background(), "gns://dir"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestListDirRetriesRequestTimeout(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			time.Sleep(30 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
	}))
	defer server.Close()

	client := mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})
	client.http.Timeout = 10 * time.Millisecond
	if _, err := client.listDir(context.Background(), "gns://dir"); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestTransientReadTimeoutBacksOffToSlowAttempt(t *testing.T) {
	if got := transientReadTimeout(0); got != transientReadFastTimeout {
		t.Fatalf("first read timeout = %s, want %s", got, transientReadFastTimeout)
	}
	for _, attempt := range []int{1, 2} {
		if got := transientReadTimeout(attempt); got != transientReadSlowTimeout {
			t.Fatalf("read timeout for attempt %d = %s, want %s", attempt, got, transientReadSlowTimeout)
		}
	}
}

func TestDirectoryDeleteUsesLongerHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	client := mustNewAPIClient(t, context.Background(), server.URL, &staticTokenProvider{token: "test-token"})
	client.http.Timeout = 5 * time.Millisecond
	client.recursiveDeleteHTTP.Timeout = 100 * time.Millisecond

	if err := client.deleteEntry(context.Background(), "gns://dir", true); err != nil {
		t.Fatalf("directory delete unexpectedly used the short API timeout: %v", err)
	}
	if err := client.deleteEntry(context.Background(), "gns://file", false); err == nil {
		t.Fatal("file delete unexpectedly ignored the short API timeout")
	}
}

func TestAPIClientUsesRcloneHTTPConfig(t *testing.T) {
	ctx, ci := fs.AddConfig(context.Background())
	ci.UserAgent = "pkudisk-api-fshttp-test"
	ci.Headers = []*fs.HTTPOption{{Key: "X-PKUDisk-Test", Value: "api"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != ci.UserAgent {
			t.Fatalf("user agent = %q, want %q", got, ci.UserAgent)
		}
		if got := r.Header.Get("X-PKUDisk-Test"); got != "api" {
			t.Fatalf("custom header = %q, want api", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"dirs": []any{}, "files": []any{}})
	}))
	defer server.Close()

	client := mustNewAPIClient(t, ctx, server.URL, &staticTokenProvider{token: "test-token"})
	if _, err := client.listDir(ctx, "gns://dir"); err != nil {
		t.Fatal(err)
	}
}

func TestObjectHTTPClientUsesRcloneTransportWithoutWholeTransferTimeout(t *testing.T) {
	ctx, ci := fs.AddConfig(context.Background())
	ci.UserAgent = "pkudisk-object-fshttp-test"
	ci.Headers = []*fs.HTTPOption{{Key: "X-PKUDisk-Test", Value: "object"}}
	ci.InsecureSkipVerify = true

	client, err := newObjectHTTPClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*fshttp.Transport)
	if !ok {
		t.Fatalf("object transport type = %T, want *fshttp.Transport", client.Transport)
	}
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("object transport lost rclone InsecureSkipVerify setting")
	}
	if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("object TLS minimum = %d, want TLS 1.2 or newer", transport.TLSClientConfig.MinVersion)
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("object transport has no CA pool after TrustAsia augmentation")
	}
	if client.Timeout != 0 {
		t.Fatalf("object client timeout = %s, want no wall-clock timeout", client.Timeout)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != ci.UserAgent {
			t.Fatalf("user agent = %q, want %q", got, ci.UserAgent)
		}
		if got := r.Header.Get("X-PKUDisk-Test"); got != "object" {
			t.Fatalf("custom header = %q, want object", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestObjectHTTPClientPreservesRcloneCACertPool(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	certFile := filepath.Join(t.TempDir(), "test-ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, ci := fs.AddConfig(context.Background())
	ci.InsecureSkipVerify = false
	ci.CaCert = []string{certFile}
	client, err := newObjectHTTPClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("object client lost rclone --ca-cert trust while adding TrustAsia CA: %v", err)
	}
	resp.Body.Close()
}
