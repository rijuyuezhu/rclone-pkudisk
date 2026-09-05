package pkudisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/rclone/rclone/fs"
)

const defaultChunkWriterConcurrency = 4

// pkudiskChunkWriter maps rclone's native multi-thread upload interface onto
// AnyShare's multipart object-storage protocol. rclone owns the scheduling and
// range reads; this type only uploads independently-addressable parts and
// finalizes the server-side multipart session.
type pkudiskChunkWriter struct {
	api         *apiClient
	objectHTTP  *http.Client
	init        multipartInit
	existingRev string
	size        int64
	partSize    int64
	partCount   int64
	signedParts multipartSignedParts

	mu       sync.Mutex
	partInfo map[string][]any
	closed   bool
	aborted  bool
}

// OpenChunkWriter starts an AnyShare multipart upload suitable for rclone's
// generic multi-thread copy engine.
func (f *Fs) OpenChunkWriter(ctx context.Context, remote string, src fs.ObjectInfo, _ ...fs.OpenOption) (info fs.ChunkWriterInfo, writer fs.ChunkWriter, err error) {
	size := src.Size()
	if size <= 0 {
		return info, nil, &fs.FileTooSmallError{MinSize: 1}
	}

	leaf, parentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return info, nil, err
	}
	if parentID == virtualRootID {
		return info, nil, errors.New("files cannot be uploaded directly into the PKU Disk virtual root")
	}

	var existingID, existingRev string
	existing, existingErr := f.NewObject(ctx, remote)
	switch {
	case existingErr == nil:
		o := existing.(*Object)
		existingID, existingRev = o.id, o.rev
	case errors.Is(existingErr, fs.ErrorObjectNotFound):
		// New file.
	default:
		return info, nil, existingErr
	}

	partSize, err := f.api.multipartPartSize(ctx, size)
	if err != nil {
		return info, nil, err
	}
	partCount := (size + partSize - 1) / partSize

	var init multipartInit
	if err := f.api.doJSON(ctx, http.MethodPost, "efast/v1/file/osinitmultiupload", uploadStartBody(
		parentID,
		f.encodeName(leaf),
		existingID,
		existingRev,
		size,
		src.ModTime(ctx),
	), &init); err != nil {
		return info, nil, err
	}
	if init.DocID == "" || init.Rev == "" || init.UploadID == "" {
		return info, nil, errors.New("PKU Disk multipart init response is incomplete")
	}

	var signedParts multipartSignedParts
	if err := f.api.doJSON(ctx, http.MethodPost, "efast/v1/file/osuploadpart", map[string]any{
		"docid":    init.DocID,
		"rev":      init.Rev,
		"uploadid": init.UploadID,
		"parts":    fmt.Sprintf("1-%d", partCount),
	}, &signedParts); err != nil {
		return info, nil, err
	}
	for part := int64(1); part <= partCount; part++ {
		if len(signedParts.AuthRequests[strconv.FormatInt(part, 10)]) < 2 {
			return info, nil, fmt.Errorf("PKU Disk multipart response is missing signed request for part %d", part)
		}
	}

	objectHTTP, err := newObjectHTTPClient()
	if err != nil {
		return info, nil, err
	}

	cw := &pkudiskChunkWriter{
		api:         f.api,
		objectHTTP:  objectHTTP,
		init:        init,
		existingRev: existingRev,
		size:        size,
		partSize:    partSize,
		partCount:   partCount,
		signedParts: signedParts,
		partInfo:    make(map[string][]any, partCount),
	}
	return fs.ChunkWriterInfo{
		ChunkSize:   partSize,
		Concurrency: defaultChunkWriterConcurrency,
		// No abort endpoint has been observed in the AnyShare desktop client or
		// public EFAST upload protocol. Be explicit that completed parts may be
		// left server-side when a transfer is interrupted; resumable state is a
		// separate concern and must not be faked by deleting the destination.
		LeavePartsOnError: true,
	}, cw, nil
}

func (w *pkudiskChunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	if chunkNumber < 0 || int64(chunkNumber) >= w.partCount {
		return 0, fmt.Errorf("invalid PKU Disk multipart chunk %d", chunkNumber)
	}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, errors.New("PKU Disk multipart writer is already closed")
	}
	if w.aborted {
		w.mu.Unlock()
		return 0, errors.New("PKU Disk multipart writer is aborted")
	}
	w.mu.Unlock()

	part := int64(chunkNumber) + 1
	partKey := strconv.FormatInt(part, 10)
	signed, err := parseHeaderSignedRequest(w.signedParts.AuthRequests[partKey])
	if err != nil {
		return 0, fmt.Errorf("multipart part %d: %w", part, err)
	}

	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek multipart part %d: %w", part, err)
	}
	expected := min(w.partSize, w.size-int64(chunkNumber)*w.partSize)
	req, err := http.NewRequestWithContext(ctx, signed.method, signed.url, io.LimitReader(reader, expected))
	if err != nil {
		return 0, err
	}
	req.ContentLength = expected
	req.Header = signed.headers.Clone()
	resp, err := w.objectHTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("upload multipart part %d: %w", part, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("upload multipart part %d: HTTP %d", part, resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return 0, fmt.Errorf("upload multipart part %d: missing ETag", part)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.aborted {
		return 0, errors.New("PKU Disk multipart writer changed state while uploading a part")
	}
	w.partInfo[partKey] = []any{etag, expected}
	return expected, nil
}

func (w *pkudiskChunkWriter) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.aborted {
		w.mu.Unlock()
		return errors.New("cannot close an aborted PKU Disk multipart upload")
	}
	if w.closed {
		w.mu.Unlock()
		return errors.New("PKU Disk multipart writer is already closed")
	}
	if int64(len(w.partInfo)) != w.partCount {
		got := len(w.partInfo)
		w.mu.Unlock()
		return fmt.Errorf("PKU Disk multipart upload has %d/%d completed parts", got, w.partCount)
	}
	partInfo := make(map[string][]any, len(w.partInfo))
	for key, value := range w.partInfo {
		partInfo[key] = append([]any(nil), value...)
	}
	w.closed = true
	w.mu.Unlock()

	_, err := w.api.finishMultipartUpload(ctx, w.init, w.existingRev, partInfo, w.objectHTTP)
	return err
}

func (w *pkudiskChunkWriter) Abort(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.aborted = true
	// AnyShare does not expose an observed safe multipart-abort operation.
	// In particular, deleting init.DocID would be unsafe for an interrupted
	// update because it may be the ID of the pre-existing destination object.
	return nil
}
