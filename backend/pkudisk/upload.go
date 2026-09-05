package pkudisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultMultipartPartSize int64 = 20 << 20

type uploadBegin struct {
	DocID       string   `json:"docid"`
	Rev         string   `json:"rev"`
	AuthRequest []string `json:"authrequest"`
}

type multipartInit struct {
	DocID    string `json:"docid"`
	Rev      string `json:"rev"`
	UploadID string `json:"uploadid"`
}

type multipartOptions struct {
	PartMinSize int64 `json:"partminsize"`
	PartMaxSize int64 `json:"partmaxsize"`
	PartMaxNum  int64 `json:"partmaxnum"`
}

type multipartSignedParts struct {
	AuthRequests map[string][]string `json:"authrequests"`
}

func (c *apiClient) signMultipartParts(ctx context.Context, init multipartInit, ranges []string) (multipartSignedParts, error) {
	result := multipartSignedParts{AuthRequests: make(map[string][]string)}
	for _, parts := range ranges {
		var batch multipartSignedParts
		if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osuploadpart", map[string]any{
			"docid":    init.DocID,
			"rev":      init.Rev,
			"uploadid": init.UploadID,
			"parts":    parts,
		}, &batch); err != nil {
			return multipartSignedParts{}, err
		}
		for part, request := range batch.AuthRequests {
			result.AuthRequests[part] = request
		}
	}
	return result, nil
}

func (c *apiClient) refreshMultipartUpload(ctx context.Context, init multipartInit, size int64) (multipartInit, error) {
	var refreshed struct {
		UploadID string `json:"uploadid"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osuploadrefresh", map[string]any{
		"docid":       init.DocID,
		"rev":         init.Rev,
		"length":      size,
		"multiupload": true,
	}, &refreshed); err != nil {
		return multipartInit{}, err
	}
	if refreshed.UploadID == "" {
		return multipartInit{}, errors.New("PKU Disk multipart refresh response is missing uploadid")
	}
	init.UploadID = refreshed.UploadID
	return init, nil
}

type signedRequest struct {
	method  string
	url     string
	headers http.Header
	fields  map[string]string
}

func (c *apiClient) upload(ctx context.Context, parentID, name, existingID, existingRev string, size int64, modTime time.Time, in io.Reader) (fileMetadata, error) {
	if size < 0 {
		return fileMetadata{}, errors.New("PKU Disk backend requires a known upload size")
	}
	if size <= defaultMultipartPartSize {
		return c.uploadSingle(ctx, parentID, name, existingID, existingRev, size, modTime, in)
	}
	return c.uploadMultipart(ctx, parentID, name, existingID, existingRev, size, modTime, in)
}

func uploadStartBody(parentID, name, existingID, existingRev string, size int64, modTime time.Time) map[string]any {
	body := map[string]any{
		"client_mtime": modTime.UnixMicro(),
		"length":       size,
	}
	if existingID != "" {
		body["docid"] = existingID
		// AnyShare's edit protocol creates a new version when name is absent.
		// editedrev gives us optimistic concurrency instead of silently
		// overwriting a version produced by another client.
		if existingRev != "" {
			body["editedrev"] = existingRev
		}
	} else {
		body["docid"] = parentID
		body["name"] = name
		body["ondup"] = 1
	}
	return body
}

func uploadEndBody(docID, rev, existingRev string) map[string]any {
	body := map[string]any{
		"docid":    docID,
		"rev":      rev,
		"csflevel": 0,
	}
	if existingRev != "" {
		body["editedrev"] = existingRev
	}
	return body
}

func (c *apiClient) uploadSingle(ctx context.Context, parentID, name, existingID, existingRev string, size int64, modTime time.Time, in io.Reader) (fileMetadata, error) {
	var begin uploadBegin
	body := uploadStartBody(parentID, name, existingID, existingRev, size, modTime)
	body["reqmethod"] = "POST"
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osbeginupload", body, &begin); err != nil {
		return fileMetadata{}, err
	}
	if begin.DocID == "" || begin.Rev == "" {
		return fileMetadata{}, errors.New("PKU Disk upload response is missing docid/rev")
	}
	signed, err := parseFormSignedRequest(begin.AuthRequest)
	if err != nil {
		return fileMetadata{}, err
	}
	if err := c.executeFormUpload(ctx, signed, name, size, in); err != nil {
		return fileMetadata{}, err
	}
	if _, err := c.do(ctx, http.MethodPost, "efast/v1/file/osendupload", uploadEndBody(begin.DocID, begin.Rev, existingRev)); err != nil {
		return fileMetadata{}, err
	}
	return c.metadata(ctx, begin.DocID)
}

func (c *apiClient) uploadMultipart(ctx context.Context, parentID, name, existingID, existingRev string, size int64, modTime time.Time, in io.Reader) (fileMetadata, error) {
	partSize, err := c.multipartPartSize(ctx, size)
	if err != nil {
		return fileMetadata{}, err
	}
	partCount := (size + partSize - 1) / partSize

	var init multipartInit
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osinitmultiupload", uploadStartBody(parentID, name, existingID, existingRev, size, modTime), &init); err != nil {
		return fileMetadata{}, err
	}
	if init.DocID == "" || init.Rev == "" || init.UploadID == "" {
		return fileMetadata{}, errors.New("PKU Disk multipart init response is incomplete")
	}

	var signedParts multipartSignedParts
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osuploadpart", map[string]any{
		"docid":    init.DocID,
		"rev":      init.Rev,
		"uploadid": init.UploadID,
		"parts":    fmt.Sprintf("1-%d", partCount),
	}, &signedParts); err != nil {
		return fileMetadata{}, err
	}

	partInfo := make(map[string][]any, partCount)
	remaining := size
	for part := int64(1); part <= partCount; part++ {
		auth := signedParts.AuthRequests[strconv.FormatInt(part, 10)]
		signed, err := parseHeaderSignedRequest(auth)
		if err != nil {
			return fileMetadata{}, fmt.Errorf("multipart part %d: %w", part, err)
		}
		chunkSize := min(partSize, remaining)
		req, err := http.NewRequestWithContext(ctx, signed.method, signed.url, io.LimitReader(in, chunkSize))
		if err != nil {
			return fileMetadata{}, err
		}
		req.ContentLength = chunkSize
		req.Header = signed.headers.Clone()
		resp, err := c.objectHTTP.Do(req)
		if err != nil {
			return fileMetadata{}, fmt.Errorf("upload multipart part %d: %w", part, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fileMetadata{}, fmt.Errorf("upload multipart part %d: HTTP %d", part, resp.StatusCode)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return fileMetadata{}, fmt.Errorf("upload multipart part %d: missing ETag", part)
		}
		partInfo[strconv.FormatInt(part, 10)] = []any{etag, chunkSize}
		remaining -= chunkSize
	}
	if remaining != 0 {
		return fileMetadata{}, fmt.Errorf("multipart upload consumed unexpected byte count: %d remain", remaining)
	}

	return c.finishMultipartUpload(ctx, init, existingRev, partInfo, c.objectHTTP)
}

func (c *apiClient) finishMultipartUpload(ctx context.Context, init multipartInit, existingRev string, partInfo map[string][]any, objectHTTP *http.Client) (fileMetadata, error) {
	completion, err := c.do(ctx, http.MethodPost, "efast/v1/file/oscompleteupload", map[string]any{
		"partinfo": partInfo,
		"docid":    init.DocID,
		"rev":      init.Rev,
		"uploadid": init.UploadID,
	})
	if err != nil {
		return fileMetadata{}, err
	}
	completionBody, completionRequest, err := parseCompletionResponse(completion)
	if err != nil {
		return fileMetadata{}, err
	}
	signed, err := parseHeaderSignedRequest(completionRequest)
	if err != nil {
		return fileMetadata{}, err
	}
	req, err := http.NewRequestWithContext(ctx, signed.method, signed.url, bytes.NewReader(completionBody))
	if err != nil {
		return fileMetadata{}, err
	}
	req.ContentLength = int64(len(completionBody))
	req.Header = signed.headers.Clone()
	resp, err := objectHTTP.Do(req)
	if err != nil {
		return fileMetadata{}, fmt.Errorf("complete object-store multipart upload: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fileMetadata{}, fmt.Errorf("complete object-store multipart upload: HTTP %d", resp.StatusCode)
	}

	if _, err := c.do(ctx, http.MethodPost, "efast/v1/file/osendupload", uploadEndBody(init.DocID, init.Rev, existingRev)); err != nil {
		return fileMetadata{}, err
	}
	return c.metadata(ctx, init.DocID)
}

func (c *apiClient) multipartPartSize(ctx context.Context, size int64) (int64, error) {
	var opts multipartOptions
	if err := c.doJSON(ctx, http.MethodPost, "efast/v1/file/osoption", nil, &opts); err != nil {
		return 0, err
	}
	if opts.PartMinSize <= 0 || opts.PartMaxSize <= 0 || opts.PartMaxNum <= 0 {
		return 0, errors.New("PKU Disk returned invalid multipart limits")
	}
	if opts.PartMaxSize*opts.PartMaxNum < size {
		return 0, errors.New("file exceeds PKU Disk multipart upload limits")
	}
	partSize := defaultMultipartPartSize
	if partSize*opts.PartMaxNum < size {
		partSize = (size + opts.PartMaxNum - 1) / opts.PartMaxNum
	}
	partSize = max(partSize, opts.PartMinSize)
	partSize = min(partSize, opts.PartMaxSize)
	return partSize, nil
}

func parseFormSignedRequest(values []string) (signedRequest, error) {
	if len(values) < 2 {
		return signedRequest{}, errors.New("signed upload request is incomplete")
	}
	result := signedRequest{method: strings.ToUpper(values[0]), url: values[1], fields: map[string]string{}}
	if result.method != http.MethodPost || result.url == "" {
		return signedRequest{}, errors.New("signed upload request has invalid method or URL")
	}
	for _, line := range values[2:] {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || key == "" {
			return signedRequest{}, fmt.Errorf("malformed signed upload field %q", line)
		}
		result.fields[key] = value
	}
	return result, nil
}

func parseHeaderSignedRequest(values []string) (signedRequest, error) {
	if len(values) < 2 {
		return signedRequest{}, errors.New("signed object request is incomplete")
	}
	result := signedRequest{method: strings.ToUpper(values[0]), url: values[1], headers: make(http.Header)}
	if result.method == "" || result.url == "" {
		return signedRequest{}, errors.New("signed object request has invalid method or URL")
	}
	for _, line := range values[2:] {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || key == "" {
			return signedRequest{}, fmt.Errorf("malformed signed object header %q", line)
		}
		if strings.EqualFold(key, "x-as-userid") {
			continue
		}
		result.headers.Set(key, value)
	}
	return result, nil
}

func (c *apiClient) executeFormUpload(ctx context.Context, signed signedRequest, name string, size int64, in io.Reader) error {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for key, value := range signed.fields {
		if err := writer.WriteField(key, value); err != nil {
			return err
		}
	}
	if _, err := writer.CreateFormFile("file", name); err != nil {
		return err
	}
	prefixLen := buffer.Len()
	if err := writer.Close(); err != nil {
		return err
	}
	all := buffer.Bytes()
	prefix := append([]byte(nil), all[:prefixLen]...)
	suffix := append([]byte(nil), all[prefixLen:]...)
	body := io.MultiReader(bytes.NewReader(prefix), io.LimitReader(in, size), bytes.NewReader(suffix))

	req, err := http.NewRequestWithContext(ctx, signed.method, signed.url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = int64(len(prefix)) + size + int64(len(suffix))
	resp, err := c.objectHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upload to PKU object storage: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload to PKU object storage: HTTP %d", resp.StatusCode)
	}
	return nil
}

var completionBoundary = regexp.MustCompile(`(?m)\s*--[A-Za-z0-9_-]+\s*`)

func parseCompletionResponse(payload []byte) ([]byte, []string, error) {
	parts := completionBoundary.Split(string(payload), -1)
	var body string
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		var meta struct {
			AuthRequest []string `json:"authrequest"`
		}
		if json.Unmarshal([]byte(trimmed), &meta) == nil && len(meta.AuthRequest) >= 2 {
			if body == "" && i > 0 {
				body = strings.TrimSpace(parts[i-1])
			}
			if body == "" {
				return nil, nil, errors.New("multipart completion response is missing body")
			}
			return []byte(body), meta.AuthRequest, nil
		}
		body = trimmed
	}
	return nil, nil, errors.New("multipart completion response is malformed")
}
