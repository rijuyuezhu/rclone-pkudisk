package pkudisk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/lib/oauthutil"
	"golang.org/x/oauth2"
)

type lockedOAuthConfig struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *lockedOAuthConfig) Get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[key]
	return value, ok
}

func (m *lockedOAuthConfig) Set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
}

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

func TestOAuthTokenProvidersSerializeRotatingRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	var lineageMu sync.Mutex
	currentRefresh := "refresh-0"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		refreshCalls.Add(1)
		// Widen the race window: without process-wide token serialization,
		// independent sources both submit refresh-0 before either can persist
		// the rotated token.
		time.Sleep(50 * time.Millisecond)

		lineageMu.Lock()
		defer lineageMu.Unlock()
		if got := r.Form.Get("refresh_token"); got != currentRefresh {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		currentRefresh = "refresh-1"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-1",
			"refresh_token": currentRefresh,
			"token_type":    "bearer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	initial := &oauth2.Token{
		AccessToken:  "access-0",
		RefreshToken: "refresh-0",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-time.Hour),
	}
	rawToken, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	m := &lockedOAuthConfig{values: map[string]string{config.ConfigToken: string(rawToken)}}
	oauthConfig := &oauthutil.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthURL:      server.URL + "/oauth2/auth",
		TokenURL:     server.URL + "/oauth2/token",
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	_, sourceA, err := oauthutil.NewClient(context.Background(), "pkudisk", m, oauthConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, sourceB, err := oauthutil.NewClient(context.Background(), "pkudisk", m, oauthConfig)
	if err != nil {
		t.Fatal(err)
	}
	providers := []*oauthTokenProvider{{source: sourceA}, {source: sourceB}}

	start := make(chan struct{})
	errs := make(chan error, len(providers))
	var wg sync.WaitGroup
	for _, provider := range providers {
		wg.Add(1)
		go func(provider *oauthTokenProvider) {
			defer wg.Done()
			<-start
			token, err := provider.Token(context.Background(), false)
			if err == nil && token != "access-1" {
				err = fmt.Errorf("access token = %q, want access-1", token)
			}
			errs <- err
		}(provider)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", got)
	}
}

func TestOAuthTokenProvidersForceRefreshAfterServerRejection(t *testing.T) {
	var refreshCalls atomic.Int32
	var lineageMu sync.Mutex
	currentRefresh := "refresh-0"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			refreshCalls.Add(1)
			time.Sleep(50 * time.Millisecond)

			lineageMu.Lock()
			defer lineageMu.Unlock()
			if got := r.Form.Get("refresh_token"); got != currentRefresh {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			currentRefresh = "refresh-1"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "access-1",
				"refresh_token": currentRefresh,
				"token_type":    "bearer",
				"expires_in":    3600,
			})
		case "/api/probe":
			w.Header().Set("Content-Type", "application/json")
			switch r.Header.Get("Authorization") {
			case "Bearer access-0":
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":    401001001,
					"message": "access token rejected",
				})
			case "Bearer access-1":
				_, _ = w.Write([]byte(`{"ok":true}`))
			default:
				w.WriteHeader(http.StatusUnauthorized)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// The key regression condition is a token that is still locally valid but
	// has already been rejected by the server.
	initial := &oauth2.Token{
		AccessToken:  "access-0",
		RefreshToken: "refresh-0",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(time.Hour),
	}
	rawToken, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	m := &lockedOAuthConfig{values: map[string]string{config.ConfigToken: string(rawToken)}}
	oauthConfig := &oauthutil.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		AuthURL:      server.URL + "/oauth2/auth",
		TokenURL:     server.URL + "/oauth2/token",
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	_, sourceA, err := oauthutil.NewClient(context.Background(), "pkudisk", m, oauthConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, sourceB, err := oauthutil.NewClient(context.Background(), "pkudisk", m, oauthConfig)
	if err != nil {
		t.Fatal(err)
	}
	clients := make([]*apiClient, 0, 2)
	for _, source := range []*oauthutil.TokenSource{sourceA, sourceB} {
		client, err := newAPIClient(context.Background(), server.URL, &oauthTokenProvider{source: source})
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}

	start := make(chan struct{})
	errs := make(chan error, len(clients))
	var wg sync.WaitGroup
	for _, client := range clients {
		wg.Add(1)
		go func(client *apiClient) {
			defer wg.Done()
			<-start
			_, err := client.do(context.Background(), http.MethodGet, "probe", nil)
			errs <- err
		}(client)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", got)
	}
	persisted, err := oauthutil.GetToken("pkudisk", m)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "access-1" || persisted.RefreshToken != "refresh-1" {
		t.Fatalf("persisted token lineage = access %q refresh %q, want access-1/refresh-1", persisted.AccessToken, persisted.RefreshToken)
	}
}
