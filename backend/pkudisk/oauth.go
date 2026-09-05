package pkudisk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/lib/oauthutil"
	"golang.org/x/oauth2"
)

const oauthScope = "offline openid all"

type oauthTokenProvider struct {
	source *oauthutil.TokenSource
}

func (p *oauthTokenProvider) Token(_ context.Context, refresh bool) (string, error) {
	if refresh {
		p.source.Invalidate()
	}
	token, err := p.source.Token()
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

func configureOAuth(ctx context.Context, name string, m configmap.Mapper, in fs.ConfigIn) (*fs.ConfigOut, error) {
	auth, _ := m.Get("auth")
	if auth != "" && !strings.EqualFold(auth, "oauth") {
		return nil, nil
	}

	switch in.State {
	case "":
		clientID, _ := m.Get("oauth_client_id")
		clientSecret, _ := m.Get("oauth_client_secret")
		udid, _ := m.Get("oauth_udid")
		if clientID == "" || clientSecret == "" || udid == "" {
			baseURL, _ := m.Get("base_url")
			if baseURL == "" {
				baseURL = defaultBaseURL
			}
			registeredID, registeredSecret, registeredUDID, err := registerOAuthClient(ctx, baseURL)
			if err != nil {
				return nil, err
			}
			m.Set("oauth_client_id", registeredID)
			m.Set("oauth_client_secret", registeredSecret)
			m.Set("oauth_udid", registeredUDID)
			clientID, clientSecret, udid = registeredID, registeredSecret, registeredUDID
		}
		return oauthutil.ConfigOut("oauth-done", &oauthutil.Options{
			OAuth2Config: buildOAuthConfig(m, clientID, clientSecret),
			NoOffline:    true,
			OAuth2Opts: []oauth2.AuthCodeOption{
				oauth2.SetAuthURLParam("audience", ""),
				oauth2.SetAuthURLParam("lang", "zh-cn"),
				oauth2.SetAuthURLParam("udids", udid),
			},
		})
	case "oauth-done":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown PKU Disk OAuth config state %q", in.State)
	}
}

func buildOAuthConfig(m configmap.Getter, clientID, clientSecret string) *oauthutil.Config {
	baseURL, _ := m.Get("base_url")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &oauthutil.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthURL:      baseURL + "/oauth2/auth",
		TokenURL:     baseURL + "/oauth2/token",
		Scopes:       strings.Fields(oauthScope),
		RedirectURL:  oauthutil.RedirectURL,
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
}

func registerOAuthClient(ctx context.Context, baseURL string) (clientID, clientSecret, udid string, err error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", "", "", errors.New("empty PKU Disk OAuth base URL")
	}
	udidBytes := make([]byte, 16)
	if _, err := rand.Read(udidBytes); err != nil {
		return "", "", "", fmt.Errorf("create OAuth device id: %w", err)
	}
	udid = hex.EncodeToString(udidBytes)
	payload := map[string]any{
		"grant_types":               []string{"authorization_code", "refresh_token", "implicit"},
		"response_types":            []string{"token id_token", "code", "token"},
		"scope":                     oauthScope,
		"redirect_uris":             []string{oauthutil.RedirectURL},
		"post_logout_redirect_uris": []string{"http://127.0.0.1:53682/logout/callback"},
		"client_name":               "rclone-pkudisk",
		"metadata": map[string]any{
			"device": map[string]any{
				"name":        "rclone-pkudisk",
				"client_type": "linux",
				"description": "rclone backend for Peking University PKU Disk",
			},
		},
		"login_form": map[string]any{"remember_password_visible": false},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/oauth2/clients", bytes.NewReader(encoded))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := fshttp.NewClient(ctx)
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("register PKU Disk OAuth client: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("register PKU Disk OAuth client: HTTP %d", resp.StatusCode)
	}
	var result struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", "", fmt.Errorf("decode PKU Disk OAuth registration: %w", err)
	}
	if result.ClientID == "" || result.ClientSecret == "" {
		return "", "", "", errors.New("PKU Disk OAuth registration returned no client credentials")
	}
	return result.ClientID, result.ClientSecret, udid, nil
}
