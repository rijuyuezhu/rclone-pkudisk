package pkudisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
)

const defaultUploadConcurrency = 4

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
	// resumeSource is retained only for local sources. It lets completed-part
	// verification read the current local bytes at WriteChunk time without
	// consuming rclone's accounted transfer reader, so user-visible transferred
	// bytes continue to represent the network data that actually needs upload.
	resumeSource fs.Object

	mu       sync.Mutex
	partInfo map[string][]any
	closed   bool
	aborted  bool
	// resumeInvalid distinguishes an ordinary Abort from a content mismatch
	// that must force rclone to retry the whole copy with a fresh multipart
	// session. Concurrent part writers must all surface a retryable error once
	// this is set so errgroup ordering cannot hide the restart request.
	resumeInvalid bool

	resumeStore *multipartResumeStore
	resumeState *multipartResumeState
}

// OpenChunkWriter starts an AnyShare multipart upload suitable for rclone's
// generic multi-thread copy engine.
func (f *Fs) OpenChunkWriter(ctx context.Context, remote string, src fs.ObjectInfo, _ ...fs.OpenOption) (info fs.ChunkWriterInfo, writer fs.ChunkWriter, err error) {
	size := src.Size()
	if size <= 0 {
		return info, nil, &fs.FileTooSmallError{MinSize: 1}
	}
	modTime := src.ModTime(ctx)
	sourceIdentity := multipartResumeSourceIdentity(ctx, src)
	resumeStore := f.openMultipartResumeStore(ctx, remote)
	handedOffStore := false
	defer func() {
		if !handedOffStore {
			resumeStore.release()
		}
	}()

	leaf, parentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return info, nil, err
	}
	if parentID == virtualRootID {
		return info, nil, errors.New("files cannot be uploaded directly into the PKU Disk virtual root")
	}
	encodedLeaf := f.encodeName(leaf)

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
	startFresh := func() (multipartInit, error) {
		var fresh multipartInit
		if err := f.api.doJSON(ctx, http.MethodPost, "efast/v1/file/osinitmultiupload", uploadStartBody(
			parentID,
			encodedLeaf,
			existingID,
			existingRev,
			size,
			modTime,
		), &fresh); err != nil {
			return multipartInit{}, err
		}
		if fresh.DocID == "" || fresh.Rev == "" || fresh.UploadID == "" {
			return multipartInit{}, errors.New("PKU Disk multipart init response is incomplete")
		}
		return fresh, nil
	}

	var state *multipartResumeState
	state, stateErr := resumeStore.load()
	if stateErr != nil {
		fs.Debugf(f, "discarding unreadable multipart resume state for %q: %v", remote, stateErr)
		_ = resumeStore.remove()
		state = nil
	}
	resumed := state != nil && state.validFor(sourceIdentity, size, modTime, parentID, encodedLeaf, existingID, existingRev, partSize, partCount)
	if state != nil && !resumed {
		fs.Debugf(f, "discarding stale multipart resume state for %q", remote)
		_ = resumeStore.remove()
		state = nil
	}

	var init multipartInit
	partInfo := make(map[string][]any, partCount)
	if resumed {
		init = state.Init
		if state.Completed == nil {
			state.Completed = make(map[string]multipartResumePart)
		}
		partInfo = state.partInfo()
		fs.Debugf(f, "resuming multipart upload %q with %d/%d completed parts", remote, len(partInfo), partCount)
	} else {
		init, err = startFresh()
		if err != nil {
			return info, nil, err
		}
		state = newMultipartResumeState(sourceIdentity, size, modTime, parentID, encodedLeaf, existingID, existingRev, partSize, partCount, init)
		if err := resumeStore.save(state); err != nil {
			fs.Debugf(f, "persist initial multipart resume state for %q: %v", remote, err)
		}
	}

	ranges := missingMultipartRanges(partCount, partInfo)
	signedParts, signErr := f.api.signMultipartParts(ctx, init, ranges)
	if signErr != nil && resumed {
		// A multipart upload ID can expire. AnyShare's osuploadrefresh creates
		// a replacement ID but does not carry completed object-store parts into
		// the new session, so refresh is a safe restart, not a partial resume.
		refreshed, refreshErr := f.api.refreshMultipartUpload(ctx, init, size)
		if refreshErr != nil {
			fs.Debugf(f, "multipart resume for %q could not be refreshed (%v); starting a fresh session", remote, refreshErr)
			_ = resumeStore.remove()
			init, err = startFresh()
			if err != nil {
				return info, nil, fmt.Errorf("resume multipart upload %q: sign remaining parts: %w; refresh failed: %v; fresh init failed: %w", remote, signErr, refreshErr, err)
			}
			partInfo = make(map[string][]any, partCount)
			state = newMultipartResumeState(sourceIdentity, size, modTime, parentID, encodedLeaf, existingID, existingRev, partSize, partCount, init)
			if err := resumeStore.save(state); err != nil {
				fs.Debugf(f, "persist replacement multipart resume state for %q: %v", remote, err)
			}
			ranges = missingMultipartRanges(partCount, partInfo)
			signedParts, signErr = f.api.signMultipartParts(ctx, init, ranges)
			if signErr != nil {
				return info, nil, signErr
			}
		} else {
			fs.Debugf(f, "multipart upload ID for %q expired; restarting incomplete upload", remote)
			init = refreshed
			partInfo = make(map[string][]any, partCount)
			state.Init = refreshed
			state.Completed = make(map[string]multipartResumePart)
			if err := resumeStore.save(state); err != nil {
				fs.Debugf(f, "persist refreshed multipart resume state for %q: %v", remote, err)
			}
			ranges = missingMultipartRanges(partCount, partInfo)
			signedParts, signErr = f.api.signMultipartParts(ctx, init, ranges)
		}
	}
	if signErr != nil {
		return info, nil, signErr
	}
	for part := int64(1); part <= partCount; part++ {
		key := strconv.FormatInt(part, 10)
		if _, done := partInfo[key]; done {
			continue
		}
		if len(signedParts.AuthRequests[key]) < 2 {
			return info, nil, fmt.Errorf("PKU Disk multipart response is missing signed request for part %d", part)
		}
	}

	cw := &pkudiskChunkWriter{
		api:         f.api,
		objectHTTP:  f.api.objectHTTP,
		init:        init,
		existingRev: existingRev,
		size:        size,
		partSize:    partSize,
		partCount:   partCount,
		signedParts: signedParts,
		partInfo:    partInfo,
		resumeStore: resumeStore,
		resumeState: state,
	}
	if sourceFs := src.Fs(); sourceFs != nil && sourceFs.Features().IsLocal {
		if obj, ok := src.(fs.Object); ok {
			cw.resumeSource = obj
		}
	}
	handedOffStore = true
	concurrency := f.opt.UploadConcurrency
	if concurrency < 1 {
		concurrency = defaultUploadConcurrency
	}
	return fs.ChunkWriterInfo{
		ChunkSize:   partSize,
		Concurrency: concurrency,
		// Ask rclone to call Abort on transfer failure so the cross-process
		// state lock is released. Abort intentionally preserves completed parts
		// and resume state because AnyShare has no observed safe abort endpoint.
		LeavePartsOnError: false,
	}, cw, nil
}

func (w *pkudiskChunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	if chunkNumber < 0 || int64(chunkNumber) >= w.partCount {
		return 0, fmt.Errorf("invalid PKU Disk multipart chunk %d", chunkNumber)
	}
	part := int64(chunkNumber) + 1
	partKey := strconv.FormatInt(part, 10)
	expected := min(w.partSize, w.size-int64(chunkNumber)*w.partSize)

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, errors.New("PKU Disk multipart writer is already closed")
	}
	if w.aborted {
		resumeInvalid := w.resumeInvalid
		w.mu.Unlock()
		if resumeInvalid {
			return 0, fserrors.RetryErrorf("PKU Disk multipart resume source changed; restarting upload")
		}
		return 0, errors.New("PKU Disk multipart writer is aborted")
	}
	var persisted multipartResumePart
	done := false
	if w.resumeState != nil {
		persisted, done = w.resumeState.Completed[partKey]
	}
	if done {
		w.mu.Unlock()
		var verifyReader io.Reader = reader
		var closeVerifyReader io.Closer
		if w.resumeSource != nil {
			start := int64(chunkNumber) * w.partSize
			r, err := w.resumeSource.Open(ctx, &fs.RangeOption{Start: start, End: start + expected - 1})
			if err != nil {
				return 0, fmt.Errorf("open completed multipart part %d for resume verification: %w", part, err)
			}
			verifyReader = r
			closeVerifyReader = r
		} else if _, err := reader.Seek(0, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seek completed multipart part %d for resume verification: %w", part, err)
		}
		h := sha256.New()
		n, err := io.CopyN(h, verifyReader, expected)
		if closeVerifyReader != nil {
			closeErr := closeVerifyReader.Close()
			if err == nil && closeErr != nil {
				err = closeErr
			}
		}
		if err != nil {
			return 0, fmt.Errorf("hash completed multipart part %d for resume verification after %d bytes: %w", part, n, err)
		}
		if hex.EncodeToString(h.Sum(nil)) != persisted.SHA256 {
			w.mu.Lock()
			w.aborted = true
			w.resumeInvalid = true
			w.mu.Unlock()
			if err := w.resumeStore.remove(); err != nil {
				fs.Debugf(nil, "remove changed-source multipart resume state: %v", err)
			}
			return 0, fserrors.RetryErrorf("PKU Disk multipart resume source changed in completed part %d; restarting upload", part)
		}
		fs.Debugf(nil, "PKU Disk multipart resume: verified completed part %d/%d", part, w.partCount)
		return expected, nil
	}
	w.mu.Unlock()

	signed, err := parseHeaderSignedRequest(w.signedParts.AuthRequests[partKey])
	if err != nil {
		return 0, fmt.Errorf("multipart part %d: %w", part, err)
	}

	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek multipart part %d: %w", part, err)
	}
	h := sha256.New()
	limited := &io.LimitedReader{R: reader, N: expected}
	req, err := http.NewRequestWithContext(ctx, signed.method, signed.url, io.TeeReader(limited, h))
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
	if limited.N != 0 {
		return 0, fmt.Errorf("upload multipart part %d: source ended %d bytes early", part, limited.N)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return 0, fmt.Errorf("upload multipart part %d: missing ETag", part)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.aborted {
		if w.resumeInvalid {
			return 0, fserrors.RetryErrorf("PKU Disk multipart resume source changed; restarting upload")
		}
		return 0, errors.New("PKU Disk multipart writer changed state while uploading a part")
	}
	w.partInfo[partKey] = []any{etag, expected}
	if w.resumeState != nil {
		w.resumeState.Completed[partKey] = multipartResumePart{
			ETag:   etag,
			Size:   expected,
			SHA256: hex.EncodeToString(h.Sum(nil)),
		}
		if err := w.resumeStore.save(w.resumeState); err != nil {
			fs.Debugf(nil, "persist PKU Disk multipart resume state after part %d: %v", part, err)
		}
	}
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
	if err != nil {
		// Once finalization starts, a failure can be ambiguous: the object
		// store may already have consumed the multipart upload ID while the
		// AnyShare osendupload step did not finish. Do not carry that state into
		// another process and risk a permanently unrecoverable completion loop.
		if removeErr := w.resumeStore.remove(); removeErr != nil {
			fs.Debugf(nil, "discard ambiguous PKU Disk multipart finalization state: %v", removeErr)
		}
		w.resumeStore.release()
		return err
	}
	if err := w.resumeStore.remove(); err != nil {
		fs.Debugf(nil, "remove completed PKU Disk multipart resume state: %v", err)
	}
	w.resumeStore.release()
	return nil
}

func (w *pkudiskChunkWriter) Abort(context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.aborted = true
	}
	w.mu.Unlock()
	// AnyShare does not expose an observed safe multipart-abort operation.
	// In particular, deleting init.DocID would be unsafe for an interrupted
	// update because it may be the ID of the pre-existing destination object.
	// Persisted completed-part state is deliberately retained for the next run.
	w.resumeStore.release()
	return nil
}
