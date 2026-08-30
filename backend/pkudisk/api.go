package pkudisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://disk.pku.edu.cn"

var expiredTokenCodes = map[int64]struct{}{
	401001001: {},
	401001002: {},
	401001003: {},
	401001004: {},
	401001005: {},
}

type apiClient struct {
	baseURL string
	http    *http.Client
	tokens  tokenProvider
}

type library struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ModifiedAt string `json:"modified_at"`
}

type entry struct {
	DocID    string `json:"docid"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	Rev      string `json:"rev"`
}

type directoryListing struct {
	Dirs  []entry `json:"dirs"`
	Files []entry `json:"files"`
}

type fileMetadata struct {
	DocID      string `json:"docid"`
	Name       string `json:"file_name"`
	NameAlt    string `json:"name"`
	Size       int64  `json:"size"`
	Modified   int64  `json:"modified"`
	Rev        string `json:"rev"`
	DocLibType string `json:"doc_lib_type"`
}

func (m fileMetadata) fileName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.NameAlt
}

type mutationResult struct {
	DocID string `json:"docid"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Rev   string `json:"rev"`
}

func (r mutationResult) docID() string {
	if r.DocID != "" {
		return r.DocID
	}
	return r.ID
}

type apiErrorEnvelope struct {
	Code    json.RawMessage `json:"code"`
	ErrCode json.RawMessage `json:"errcode"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Detail  string          `json:"detail"`
	Error   string          `json:"error"`
}

type apiError struct {
	Status int
	Code   int64
	Msg    string
}

func (e *apiError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("PKU Disk API error: HTTP %d, code %d: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("PKU Disk API error: HTTP %d: %s", e.Status, e.Msg)
}

func newAPIClient(baseURL string, tokens tokenProvider) *apiClient {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
		tokens: tokens,
	}
}

func (c *apiClient) endpoint(path string) string {
	return c.baseURL + "/api/" + strings.TrimLeft(path, "/")
}

func (c *apiClient) do(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.tokens.Token(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.endpoint(endpoint), reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("PKU Disk request failed: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		apiErr := parseAPIError(resp.StatusCode, data)
		if apiErr == nil {
			return data, nil
		}
		if attempt == 0 && isAuthError(apiErr) {
			continue
		}
		return nil, apiErr
	}
	return nil, errors.New("PKU Disk authentication failed after token refresh")
}

func (c *apiClient) doJSON(ctx context.Context, method, endpoint string, body, out any) error {
	data, err := c.do(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode PKU Disk response from %s: %w", endpoint, err)
	}
	return nil
}

func parseAPIError(status int, data []byte) error {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(data, &envelope)
	code := parseErrorCode(envelope.Code)
	if code == 0 {
		code = parseErrorCode(envelope.ErrCode)
	}
	msg := firstNonEmpty(envelope.Message, envelope.Msg, envelope.Detail, envelope.Error)
	if msg == "" {
		msg = http.StatusText(status)
	}
	if status >= 400 || code >= 400000000 {
		return &apiError{Status: status, Code: code, Msg: msg}
	}
	return nil
}

func parseErrorCode(raw json.RawMessage) int64 {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		_, _ = fmt.Sscan(s, &n)
	}
	return n
}

func isAuthError(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status == http.StatusUnauthorized {
		return true
	}
	_, ok := expiredTokenCodes[apiErr.Code]
	return ok
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (c *apiClient) libraries(ctx context.Context) ([]library, error) {
	var result []library
	err := c.doJSON(ctx, http.MethodGet, "efast/v1/entry-doc-lib", nil, &result)
	return result, err
}

func (c *apiClient) listDir(ctx context.Context, docID string) (directoryListing, error) {
	var result directoryListing
	err := c.doJSON(ctx, http.MethodPost, "efast/v1/dir/list", map[string]any{
		"docid": docID,
		"by":    "name",
		"sort":  "asc",
	}, &result)
	return result, err
}

func (c *apiClient) metadata(ctx context.Context, docID string) (fileMetadata, error) {
	var result fileMetadata
	err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/metadata", map[string]any{"docid": docID}, &result)
	if result.DocID == "" {
		result.DocID = docID
	}
	return result, err
}

func (c *apiClient) createDir(ctx context.Context, parentID, name string) (string, error) {
	var result mutationResult
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/dir/create", map[string]any{
		"docid": parentID,
		"name":  name,
		"ondup": 1,
	}, &result); err != nil {
		return "", err
	}
	if result.docID() != "" {
		return result.docID(), nil
	}
	listing, err := c.listDir(ctx, parentID)
	if err != nil {
		return "", err
	}
	for _, item := range listing.Dirs {
		if item.Name == name {
			return item.DocID, nil
		}
	}
	return "", fmt.Errorf("directory %q was created but could not be resolved", name)
}

func (c *apiClient) deleteEntry(ctx context.Context, docID string, isDir bool) error {
	kind := "file"
	body := map[string]any{"docid": docID}
	if isDir {
		kind = "dir"
		body["check_upload_process"] = true
	}
	_, err := c.do(ctx, http.MethodPost, "efast/v1/"+kind+"/delete", body)
	return err
}

func (c *apiClient) renameEntry(ctx context.Context, docID, name string, isDir bool) error {
	kind := "file"
	if isDir {
		kind = "dir"
	}
	_, err := c.do(ctx, http.MethodPost, "efast/v1/"+kind+"/rename", map[string]any{
		"docid": docID,
		"name":  name,
		"ondup": 1,
	})
	return err
}

func (c *apiClient) moveEntry(ctx context.Context, docID, parentID string, isDir bool) error {
	kind := "file"
	body := map[string]any{
		"docid":      docID,
		"destparent": parentID,
		"ondup":      1,
	}
	if isDir {
		kind = "dir"
		body["check_upload_process"] = true
	}
	_, err := c.do(ctx, http.MethodPost, "efast/v1/"+kind+"/move", body)
	return err
}

func (c *apiClient) downloadURL(ctx context.Context, meta fileMetadata) (string, error) {
	var result struct {
		AuthRequest []any `json:"authrequest"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osdownload", map[string]any{
		"docid":    meta.DocID,
		"authtype": "QUERY_STRING",
		"savename": meta.fileName(),
		"usehttps": true,
		"rev":      meta.Rev,
	}, &result); err != nil {
		return "", err
	}
	if len(result.AuthRequest) < 2 {
		return "", errors.New("PKU Disk download response is missing signed URL")
	}
	signed, ok := result.AuthRequest[1].(string)
	if !ok || signed == "" {
		return "", errors.New("PKU Disk download response contains an invalid signed URL")
	}
	if _, err := url.ParseRequestURI(signed); err != nil {
		return "", fmt.Errorf("invalid signed download URL: %w", err)
	}
	return signed, nil
}

func parseModifiedMicros(micros int64) time.Time {
	if micros <= 0 {
		return time.Time{}
	}
	return time.Unix(0, micros*int64(time.Microsecond)).UTC()
}
