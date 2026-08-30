package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

	client := newAPIClient(server.URL, provider)
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

func TestObjectHTTPClientIsSharedWithoutWholeTransferTimeout(t *testing.T) {
	client, err := newObjectHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	again, err := newObjectHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if client != again {
		t.Fatal("object HTTP client is not shared across transfers")
	}
	if client.Timeout != 0 {
		t.Fatalf("object client timeout = %s, want no wall-clock timeout", client.Timeout)
	}
}
