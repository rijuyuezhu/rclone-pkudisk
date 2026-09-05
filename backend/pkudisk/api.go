package pkudisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://disk.pku.edu.cn"

const (
	defaultAPITimeout        = 60 * time.Second
	recursiveDeleteTimeout   = 5 * time.Minute
	transientReadAttempts    = 3
	transientReadFastTimeout = 20 * time.Second
	transientReadSlowTimeout = 60 * time.Second
	transientRetryDelay      = 250 * time.Millisecond
)

var reauthenticationCodes = map[int64]struct{}{
	// These codes have been observed in the PKU AnyShare client / bridge as
	// states where the current bearer session should be reacquired once. Some
	// are policy failures rather than literal expiry; retrying once is useful
	// for auth=pkudist because the official client may already have rotated its
	// token, and remains harmless for OAuth before returning the original error.
	401001001: {},
	401001002: {},
	401001003: {},
	401001004: {},
	401001005: {},
	401001011: {},
	401001025: {},
	401001031: {},
	401001033: {},
	401001036: {},
	401001051: {},
}

type apiClient struct {
	baseURL             string
	http                *http.Client
	recursiveDeleteHTTP *http.Client
	tokens              tokenProvider
}

type library struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ModifiedAt string `json:"modified_at"`
}

type entry struct {
	DocID       string `json:"docid"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Modified    int64  `json:"modified"`
	ClientMTime int64  `json:"client_mtime"`
	Rev         string `json:"rev"`
}

type directoryListing struct {
	Dirs  []entry `json:"dirs"`
	Files []entry `json:"files"`
}

type fileMetadata struct {
	DocID       string `json:"docid"`
	Name        string `json:"file_name"`
	NameAlt     string `json:"name"`
	Size        int64  `json:"size"`
	Modified    int64  `json:"modified"`
	ClientMTime int64  `json:"client_mtime"`
	Rev         string `json:"rev"`
	DocLibType  string `json:"doc_lib_type"`
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
			Timeout: defaultAPITimeout,
		},
		recursiveDeleteHTTP: &http.Client{
			Timeout: recursiveDeleteTimeout,
		},
		tokens: tokens,
	}
}

func (c *apiClient) endpoint(path string) string {
	return c.baseURL + "/api/" + strings.TrimLeft(path, "/")
}

func (c *apiClient) do(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	return c.doWithClient(ctx, c.http, method, endpoint, body)
}

func (c *apiClient) doWithClient(ctx context.Context, client *http.Client, method, endpoint string, body any) ([]byte, error) {
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
		resp, err := client.Do(req)
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

func transientReadTimeout(attempt int) time.Duration {
	if attempt == 0 {
		return transientReadFastTimeout
	}
	return transientReadSlowTimeout
}

func (c *apiClient) doJSONWithTransientRetry(ctx context.Context, method, endpoint string, body, out any) error {
	return retryTransient(ctx, transientReadAttempts, func(attempt int) error {
		attemptCtx, cancel := context.WithTimeout(ctx, transientReadTimeout(attempt))
		defer cancel()
		return c.doJSON(attemptCtx, method, endpoint, body, out)
	})
}

func retryTransient(ctx context.Context, attempts int, fn func(attempt int) error) error {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		err = fn(attempt)
		if err == nil || !isTransientError(err) || attempt+1 == attempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(transientRetryDelay << attempt):
		}
	}
	return err
}

func isTransientError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= http.StatusInternalServerError
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
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
	_, ok := reauthenticationCodes[apiErr.Code]
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
	err := c.doJSONWithTransientRetry(ctx, http.MethodGet, "efast/v1/entry-doc-lib", nil, &result)
	return result, err
}

func (c *apiClient) listDir(ctx context.Context, docID string) (directoryListing, error) {
	var result directoryListing
	err := c.doJSONWithTransientRetry(ctx, http.MethodPost, "efast/v1/dir/list", map[string]any{
		"docid": docID,
		"by":    "name",
		"sort":  "asc",
	}, &result)
	return result, err
}

func (c *apiClient) metadata(ctx context.Context, docID string) (fileMetadata, error) {
	var result fileMetadata
	err := c.doJSONWithTransientRetry(ctx, http.MethodPost, "efast/v1/file/metadata", map[string]any{"docid": docID}, &result)
	if result.DocID == "" {
		result.DocID = docID
	}
	return result, err
}

func (c *apiClient) getInfoByPath(ctx context.Context, namePath string) (entry, error) {
	var result entry
	err := c.doJSONWithTransientRetry(ctx, http.MethodPost, "efast/v1/file/getinfobypath", map[string]any{
		"namepath": namePath,
	}, &result)
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
	client := c.http
	if isDir {
		kind = "dir"
		body["check_upload_process"] = true
		client = c.recursiveDeleteHTTP
	}
	_, err := c.doWithClient(ctx, client, http.MethodPost, "efast/v1/"+kind+"/delete", body)
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

func (c *apiClient) copyFile(ctx context.Context, docID, parentID string, ondup int) (string, error) {
	var result mutationResult
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/copy", map[string]any{
		"docid":      docID,
		"destparent": parentID,
		"ondup":      ondup,
	}, &result); err != nil {
		return "", err
	}
	if result.docID() == "" {
		return "", errors.New("PKU Disk copy response is missing docid")
	}
	return result.docID(), nil
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

func fileModTime(clientMTime, modified int64) time.Time {
	if clientMTime > 0 {
		return parseModifiedMicros(clientMTime)
	}
	return parseModifiedMicros(modified)
}
