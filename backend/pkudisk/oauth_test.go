package pkudisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/lib/oauthutil"
)

func TestConfigureOAuthRegistersOnce(t *testing.T) {
	var calls atomic.Int32
	ctx, ci := fs.AddConfig(context.Background())
	ci.UserAgent = "pkudisk-oauth-fshttp-test"
	ci.Headers = []*fs.HTTPOption{{Key: "X-PKUDisk-Test", Value: "oauth"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/clients" {
			http.NotFound(w, r)
			return
		}
		if got := r.UserAgent(); got != ci.UserAgent {
			t.Fatalf("user agent = %q, want %q", got, ci.UserAgent)
		}
		if got := r.Header.Get("X-PKUDisk-Test"); got != "oauth" {
			t.Fatalf("custom header = %q, want oauth", got)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"client_id":     "client-id",
			"client_secret": "client-secret",
		})
	}))
	defer server.Close()

	m := configmap.Simple{"auth": "oauth", "base_url": server.URL}
	out, err := configureOAuth(ctx, "pku", m, fs.ConfigIn{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.OAuth.(*oauthutil.Options); !ok {
		t.Fatalf("OAuth config type = %T", out.OAuth)
	}
	if out.State != "oauth-done" {
		t.Fatalf("OAuth return state = %q", out.State)
	}
	if got, _ := m.Get("oauth_client_id"); got != "client-id" {
		t.Fatalf("client id = %q", got)
	}
	if got, _ := m.Get("oauth_client_secret"); got != "client-secret" {
		t.Fatalf("client secret was not stored")
	}
	if got, _ := m.Get("oauth_udid"); len(got) != 32 {
		t.Fatalf("udid length = %d, want 32", len(got))
	}

	if _, err := configureOAuth(ctx, "pku", m, fs.ConfigIn{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("registration calls = %d, want 1", calls.Load())
	}
}
