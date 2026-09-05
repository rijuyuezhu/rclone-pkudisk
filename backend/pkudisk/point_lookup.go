package pkudisk

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/dircache"
)

const getInfoPathNotFoundCode int64 = 404002006

func isGetInfoPathNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == getInfoPathNotFoundCode
}

// encodedNamePath returns an AnyShare name path rooted at the top-level
// document library. The first component is a document-library display name and
// must stay untouched; normal file/directory components use the backend's
// reversible filename encoding.
func (f *Fs) encodedNamePath(remote string) string {
	full := strings.Trim(path.Join(f.root, remote), "/")
	if full == "" {
		return ""
	}
	parts := strings.Split(full, "/")
	for i := 1; i < len(parts); i++ {
		parts[i] = f.encodeName(parts[i])
	}
	return strings.Join(parts, "/")
}

// seedDirCacheFromDocID reconstructs the cumulative GNS IDs for all directory
// components from the docid returned by getinfobypath. This is only a seed for
// rclone's existing dircache; it does not introduce another metadata cache.
func seedDirCacheFromDocID(dc *dircache.DirCache, standardPath, docID string) bool {
	standardPath = strings.Trim(standardPath, "/")
	if standardPath == "" || !strings.HasPrefix(docID, "gns://") {
		return false
	}
	names := strings.Split(standardPath, "/")
	ids := strings.Split(strings.TrimPrefix(docID, "gns://"), "/")
	if len(names) != len(ids) {
		return false
	}
	for i := range names {
		namePrefix := strings.Join(names[:i+1], "/")
		idPrefix := "gns://" + strings.Join(ids[:i+1], "/")
		dc.Put(namePrefix, idPrefix)
	}
	return true
}

func parentDocID(docID string) (string, bool) {
	if !strings.HasPrefix(docID, "gns://") {
		return "", false
	}
	last := strings.LastIndex(docID, "/")
	if last <= len("gns://") {
		return "", false
	}
	return docID[:last], true
}

// tryResolveRootByPath uses getinfobypath as an optimization for non-empty
// roots. On any unsupported/ambiguous failure it returns handled=false so NewFs
// can fall back to the existing directory traversal unchanged.
func (f *Fs) tryResolveRootByPath(ctx context.Context) (handled bool, isFile bool) {
	if f.root == "" {
		return false, false
	}
	meta, err := f.api.getInfoByPath(ctx, f.encodedNamePath(""))
	if err != nil {
		return false, false
	}

	if meta.Size < 0 {
		if !seedDirCacheFromDocID(f.dirCache, f.root, meta.DocID) {
			return false, false
		}
		if err := f.dirCache.FindRoot(ctx, false); err != nil {
			return false, false
		}
		return true, false
	}

	parentRoot, _ := dircache.SplitPath(f.root)
	parentID, ok := parentDocID(meta.DocID)
	if !ok || parentRoot == "" {
		return false, false
	}
	tempF := *f
	tempF.root = parentRoot
	tempF.dirCache = dircache.New(parentRoot, virtualRootID, &tempF)
	if !seedDirCacheFromDocID(tempF.dirCache, parentRoot, parentID) {
		return false, false
	}
	if err := tempF.dirCache.FindRoot(ctx, false); err != nil {
		return false, false
	}
	f.root = parentRoot
	f.dirCache = tempF.dirCache
	return true, true
}

func (f *Fs) newObjectByPath(ctx context.Context, remote string) (fs.Object, bool, error) {
	namePath := f.encodedNamePath(remote)
	if namePath == "" {
		return nil, false, nil
	}
	meta, err := f.api.getInfoByPath(ctx, namePath)
	if err != nil {
		if isGetInfoPathNotFound(err) {
			return nil, true, fs.ErrorObjectNotFound
		}
		// Point lookup is an optimization. If it suffers an unrelated API
		// failure, retain the existing listing path as a recovery route.
		return nil, false, nil
	}
	if meta.Size < 0 {
		return nil, true, fs.ErrorIsDir
	}
	return f.objectFromEntry(remote, meta), true, nil
}
