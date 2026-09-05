// Package pkudisk implements an rclone backend for Peking University PKU Disk
// (AnyShare 7).
package pkudisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
)

const (
	virtualRootID   = "pkudisk://root"
	defaultEncoding = encoder.EncodeCtl |
		encoder.EncodeDel |
		encoder.EncodeDot |
		encoder.EncodeSlash |
		encoder.EncodeBackSlash |
		encoder.EncodeWin |
		encoder.EncodeLeftSpace |
		encoder.EncodeRightSpace |
		encoder.EncodeRightPeriod |
		encoder.EncodeInvalidUtf8
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "pkudisk",
		Description: "Peking University PKU Disk (AnyShare)",
		NewFs:       NewFs,
		Config:      configureOAuth,
		Options: []fs.Option{
			{
				Name:    "auth",
				Help:    "Authentication source. OAuth is recommended for long-running sync and mount jobs.",
				Default: "oauth",
				Examples: []fs.OptionExample{
					{Value: "oauth", Help: "Register an rclone client and sign in through PKU Disk OAuth (recommended)."},
					{Value: "pkudist", Help: "Reuse the current official PKU Disk desktop client access token."},
					{Value: "token", Help: "Use an explicit access token."},
				},
			},
			{
				Name:      "access_token",
				Help:      "PKU Disk OAuth access token when auth=token.",
				Sensitive: true,
			},
			{
				Name:     "pkudist_leveldb",
				Help:     "Path to AnyShare Chromium Local Storage LevelDB. Leave empty for the standard Linux path.",
				Advanced: true,
			},
			{
				Name:     "base_url",
				Help:     "PKU Disk service base URL.",
				Default:  defaultBaseURL,
				Advanced: true,
			},
			{
				Name:      "oauth_client_id",
				Help:      "Dynamically registered AnyShare OAuth client ID.",
				Hide:      fs.OptionHideBoth,
				Sensitive: true,
			},
			{
				Name:      "oauth_client_secret",
				Help:      "Dynamically registered AnyShare OAuth client secret.",
				Hide:      fs.OptionHideBoth,
				Sensitive: true,
			},
			{
				Name: "oauth_udid",
				Help: "AnyShare OAuth device identifier.",
				Hide: fs.OptionHideBoth,
			},
			{
				Name:     config.ConfigEncoding,
				Help:     config.ConfigEncodingHelp,
				Advanced: true,
				Default:  defaultEncoding,
			},
		},
	})
}

type Options struct {
	Auth              string               `config:"auth"`
	AccessToken       string               `config:"access_token"`
	PkudistLevelDB    string               `config:"pkudist_leveldb"`
	BaseURL           string               `config:"base_url"`
	OAuthClientID     string               `config:"oauth_client_id"`
	OAuthClientSecret string               `config:"oauth_client_secret"`
	OAuthUDID         string               `config:"oauth_udid"`
	Enc               encoder.MultiEncoder `config:"encoding"`
}

type Fs struct {
	name      string
	root      string
	opt       Options
	api       *apiClient
	dirCache  *dircache.DirCache
	features  *fs.Features
	resumeDir string
}

type Object struct {
	fs      *Fs
	remote  string
	id      string
	size    int64
	modTime time.Time
	rev     string
}

func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	var tokens tokenProvider
	switch strings.ToLower(strings.TrimSpace(opt.Auth)) {
	case "", "oauth":
		if opt.OAuthClientID == "" || opt.OAuthClientSecret == "" {
			return nil, errors.New("PKU Disk OAuth is not configured; run `rclone config` or `rclone config reconnect` first")
		}
		_, source, err := oauthutil.NewClient(ctx, name, m, buildOAuthConfig(m, opt.OAuthClientID, opt.OAuthClientSecret))
		if err != nil {
			return nil, fmt.Errorf("initialize PKU Disk OAuth: %w", err)
		}
		tokens = &oauthTokenProvider{source: source}
	case "pkudist":
		provider, err := newPkudistTokenProvider(opt.PkudistLevelDB)
		if err != nil {
			return nil, err
		}
		tokens = provider
	case "token":
		if opt.AccessToken == "" {
			return nil, errors.New("access_token is required when auth=token")
		}
		tokens = &staticTokenProvider{token: opt.AccessToken}
	default:
		return nil, fmt.Errorf("unsupported pkudisk auth mode %q", opt.Auth)
	}

	root = strings.Trim(root, "/")
	f := &Fs{
		name:      name,
		root:      root,
		opt:       *opt,
		api:       newAPIClient(opt.BaseURL, tokens),
		resumeDir: filepath.Join(config.GetCacheDir(), "pkudisk", "multipart"),
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	f.dirCache = dircache.New(root, virtualRootID, f)
	if handled, isFile := f.tryResolveRootByPath(ctx); handled {
		if isFile {
			return f, fs.ErrorIsFile
		}
		return f, nil
	}

	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.root = newRoot
		tempF.dirCache = dircache.New(newRoot, virtualRootID, &tempF)
		if rootErr := tempF.dirCache.FindRoot(ctx, false); rootErr != nil {
			return f, nil
		}
		if _, objectErr := tempF.NewObject(ctx, remote); objectErr != nil {
			if errors.Is(objectErr, fs.ErrorObjectNotFound) || errors.Is(objectErr, fs.ErrorIsDir) {
				return f, nil
			}
			return nil, objectErr
		}
		f.root = newRoot
		f.dirCache = tempF.dirCache
		return f, fs.ErrorIsFile
	}
	return f, nil
}

func (f *Fs) Name() string             { return f.name }
func (f *Fs) Root() string             { return f.root }
func (f *Fs) String() string           { return fmt.Sprintf("PKU Disk root %q", f.root) }
func (f *Fs) Features() *fs.Features   { return f.features }
func (f *Fs) Precision() time.Duration { return time.Microsecond }
func (f *Fs) Hashes() hash.Set         { return hash.Set(hash.None) }

func (f *Fs) encodeName(name string) string { return f.opt.Enc.FromStandardName(name) }
func (f *Fs) decodeName(name string) string { return f.opt.Enc.ToStandardName(name) }

func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (string, bool, error) {
	if pathID == virtualRootID {
		libs, err := f.api.libraries(ctx)
		if err != nil {
			return "", false, err
		}
		for _, lib := range libs {
			if lib.Name == leaf {
				return lib.ID, true, nil
			}
		}
		return "", false, nil
	}
	listing, err := f.api.listDir(ctx, pathID)
	if err != nil {
		return "", false, err
	}
	for _, item := range listing.Dirs {
		if f.decodeName(item.Name) == leaf {
			return item.DocID, true, nil
		}
	}
	return "", false, nil
}

func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (string, error) {
	if pathID == virtualRootID {
		return "", errors.New("PKU Disk document libraries cannot be created through the file API")
	}
	return f.api.createDir(ctx, pathID, f.encodeName(leaf))
}

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	id, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, fs.ErrorDirNotFound
	}
	entries := fs.DirEntries{}
	if id == virtualRootID {
		libs, err := f.api.libraries(ctx)
		if err != nil {
			return nil, err
		}
		for _, lib := range libs {
			remote := path.Join(dir, lib.Name)
			modTime, _ := time.Parse(time.RFC3339, lib.ModifiedAt)
			entries = append(entries, fs.NewDir(remote, modTime).SetID(lib.ID))
			f.dirCache.Put(remote, lib.ID)
		}
		return entries, nil
	}

	listing, err := f.api.listDir(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, item := range listing.Dirs {
		remote := path.Join(dir, f.decodeName(item.Name))
		entries = append(entries, fs.NewDir(remote, parseModifiedMicros(item.Modified)).SetID(item.DocID))
		f.dirCache.Put(remote, item.DocID)
	}
	for _, item := range listing.Files {
		entries = append(entries, f.objectFromEntry(path.Join(dir, f.decodeName(item.Name)), item))
	}
	return entries, nil
}

func (f *Fs) objectFromEntry(remote string, item entry) *Object {
	return &Object{
		fs:      f,
		remote:  remote,
		id:      item.DocID,
		size:    item.Size,
		modTime: fileModTime(item.ClientMTime, item.Modified),
		rev:     item.Rev,
	}
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if object, handled, err := f.newObjectByPath(ctx, remote); handled {
		return object, err
	}
	dir, leaf := dircache.SplitPath(remote)
	parentID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil || parentID == virtualRootID {
		return nil, fs.ErrorObjectNotFound
	}
	listing, err := f.api.listDir(ctx, parentID)
	if err != nil {
		return nil, err
	}
	for _, item := range listing.Files {
		if f.decodeName(item.Name) == leaf {
			return f.objectFromEntry(remote, item), nil
		}
	}
	for _, item := range listing.Dirs {
		if f.decodeName(item.Name) == leaf {
			return nil, fs.ErrorIsDir
		}
	}
	return nil, fs.ErrorObjectNotFound
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	if existing, err := f.NewObject(ctx, remote); err == nil {
		if err := existing.Update(ctx, in, src, options...); err != nil {
			return nil, err
		}
		return existing, nil
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return nil, err
	}

	o := &Object{fs: f, remote: remote}
	if err := o.Update(ctx, in, src, options...); err != nil {
		return nil, err
	}
	return o, nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if dir == "" && f.root == "" {
		return errors.New("cannot remove PKU Disk virtual root")
	}
	id, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil || id == virtualRootID {
		return fs.ErrorDirNotFound
	}
	// rclone constructs an Fs rooted at the target for `rmdir remote:path`
	// and then calls Rmdir(""). Permit that for ordinary directories, but
	// never turn a top-level document library into a deletable directory.
	if dir == "" {
		parentID, err := f.dirCache.RootParentID(ctx, false)
		if err != nil {
			return err
		}
		if parentID == virtualRootID {
			return errors.New("PKU Disk document libraries cannot be removed through the file API")
		}
	}
	listing, err := f.api.listDir(ctx, id)
	if err != nil {
		return err
	}
	if len(listing.Dirs) != 0 || len(listing.Files) != 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	if err := f.api.deleteEntry(ctx, id, true); err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// Purge removes dir and all of its contents using AnyShare's native
// recursive directory delete operation. The virtual root and document
// libraries are deliberately protected.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if dir == "" && f.root == "" {
		return errors.New("cannot purge PKU Disk virtual root")
	}
	id, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil || id == virtualRootID {
		return fs.ErrorDirNotFound
	}

	var parentID string
	if dir == "" {
		parentID, err = f.dirCache.RootParentID(ctx, false)
	} else {
		parent, _ := dircache.SplitPath(dir)
		parentID, err = f.dirCache.FindDir(ctx, parent, false)
	}
	if err != nil {
		return err
	}
	if parentID == virtualRootID {
		return errors.New("PKU Disk document libraries cannot be removed through the file API")
	}
	if err := f.api.deleteEntry(ctx, id, true); err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	source, ok := src.(*Object)
	if !ok || source.fs.name != f.name {
		return nil, fs.ErrorCantCopy
	}

	dstDir, dstLeaf := dircache.SplitPath(remote)
	dstParentID, err := f.dirCache.FindDir(ctx, dstDir, true)
	if err != nil {
		return nil, err
	}
	if dstParentID == virtualRootID {
		return nil, fs.ErrorCantCopy
	}

	_, srcLeaf := dircache.SplitPath(source.remote)
	existing, existingErr := f.NewObject(ctx, remote)
	switch {
	case existingErr == nil:
		if existing.(*Object).id == source.id {
			return existing, nil
		}
		// AnyShare's copy overwrite mode has no destination revision guard.
		// Preserve the backend's existing optimistic-concurrency semantics by
		// letting rclone use the normal Update path for replacements.
		return nil, fs.ErrorCantCopy
	case errors.Is(existingErr, fs.ErrorObjectNotFound):
		// New destination.
	case errors.Is(existingErr, fs.ErrorIsDir):
		return nil, fs.ErrorCantCopy
	default:
		return nil, existingErr
	}

	needsRename := srcLeaf != dstLeaf
	ondup := 1 // fail rather than overwrite an unexpected concurrent entry
	if needsRename {
		ondup = 2 // auto-name the temporary copy; rename it by returned docid
	}
	copiedID, err := f.api.copyFile(ctx, source.id, dstParentID, ondup)
	if err != nil {
		return nil, err
	}

	if needsRename {
		if err := f.api.renameEntry(ctx, copiedID, f.encodeName(dstLeaf), false); err != nil {
			cleanupErr := f.api.deleteEntry(ctx, copiedID, false)
			if cleanupErr != nil {
				return nil, fmt.Errorf("rename server-side copy: %w (cleanup failed: %v)", err, cleanupErr)
			}
			return nil, fmt.Errorf("rename server-side copy: %w", err)
		}
	}
	return f.NewObject(ctx, remote)
}

func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	source, ok := src.(*Object)
	if !ok || source.fs.name != f.name {
		return nil, fs.ErrorCantMove
	}
	dstDir, dstLeaf := dircache.SplitPath(remote)
	dstParentID, err := f.dirCache.FindDir(ctx, dstDir, true)
	if err != nil {
		return nil, err
	}
	srcDir, srcLeaf := dircache.SplitPath(source.remote)
	srcParentID, err := source.fs.dirCache.FindDir(ctx, srcDir, false)
	if err != nil {
		return nil, err
	}
	if _, err := f.relocateEntry(ctx, source.id, srcParentID, dstParentID, srcLeaf, dstLeaf, false, fs.ErrorCantMove); err != nil {
		return nil, err
	}
	return f.NewObject(ctx, remote)
}

func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	source, ok := src.(*Fs)
	if !ok || source.name != f.name {
		return fs.ErrorCantDirMove
	}
	if (srcRemote == "" && source.root == "") || (dstRemote == "" && f.root == "") {
		return fs.ErrorCantDirMove
	}

	srcID, srcParentID, srcLeaf, dstParentID, dstLeaf, err := f.dirCache.DirMove(
		ctx, source.dirCache, source.root, srcRemote, f.root, dstRemote,
	)
	if err != nil {
		return err
	}
	if srcParentID == virtualRootID || dstParentID == virtualRootID {
		return fs.ErrorCantDirMove
	}
	if _, err := f.relocateEntry(ctx, srcID, srcParentID, dstParentID, srcLeaf, dstLeaf, true, fs.ErrorCantDirMove); err != nil {
		return err
	}
	source.dirCache.FlushDir(srcRemote)
	f.dirCache.FlushDir(dstRemote)
	return nil
}

func (f *Fs) relocateEntry(ctx context.Context, id, srcParentID, dstParentID, srcLeaf, dstLeaf string, isDir bool, cantMove error) (string, error) {
	if srcParentID == dstParentID {
		if srcLeaf == dstLeaf {
			return id, nil
		}
		if err := f.api.renameEntry(ctx, id, f.encodeName(dstLeaf), isDir); err != nil {
			return "", err
		}
		return f.resolveChildID(ctx, srcParentID, dstLeaf, isDir)
	}

	if srcLeaf == dstLeaf {
		if err := f.api.moveEntry(ctx, id, dstParentID, isDir); err != nil {
			return "", err
		}
		return f.resolveChildID(ctx, dstParentID, dstLeaf, isDir)
	}

	dstHasSourceName, err := f.childExists(ctx, dstParentID, srcLeaf, isDir)
	if err != nil {
		return "", err
	}
	if !dstHasSourceName {
		if err := f.api.moveEntry(ctx, id, dstParentID, isDir); err != nil {
			return "", err
		}
		movedID, err := f.resolveChildID(ctx, dstParentID, srcLeaf, isDir)
		if err != nil {
			return "", err
		}
		if err := f.api.renameEntry(ctx, movedID, f.encodeName(dstLeaf), isDir); err != nil {
			return "", err
		}
		return f.resolveChildID(ctx, dstParentID, dstLeaf, isDir)
	}

	// Moving first would collide with an existing destination entry that has
	// the source name. Rename first if that intermediate name is free in the
	// source directory. If both intermediate names collide, let rclone fall
	// back instead of leaving a partially renamed server-side object.
	srcHasDestinationName, err := f.childExists(ctx, srcParentID, dstLeaf, isDir)
	if err != nil {
		return "", err
	}
	if srcHasDestinationName {
		return "", cantMove
	}
	if err := f.api.renameEntry(ctx, id, f.encodeName(dstLeaf), isDir); err != nil {
		return "", err
	}
	renamedID, err := f.resolveChildID(ctx, srcParentID, dstLeaf, isDir)
	if err != nil {
		return "", err
	}
	if err := f.api.moveEntry(ctx, renamedID, dstParentID, isDir); err != nil {
		// Best-effort rollback: keep the source path stable when the second
		// server-side operation fails.
		_ = f.api.renameEntry(ctx, renamedID, f.encodeName(srcLeaf), isDir)
		return "", err
	}
	return f.resolveChildID(ctx, dstParentID, dstLeaf, isDir)
}

func (f *Fs) childExists(ctx context.Context, parentID, leaf string, isDir bool) (bool, error) {
	listing, err := f.api.listDir(ctx, parentID)
	if err != nil {
		return false, err
	}
	entries := listing.Files
	if isDir {
		entries = listing.Dirs
	}
	for _, item := range entries {
		if f.decodeName(item.Name) == leaf {
			return true, nil
		}
	}
	return false, nil
}

func (f *Fs) resolveChildID(ctx context.Context, parentID, leaf string, isDir bool) (string, error) {
	listing, err := f.api.listDir(ctx, parentID)
	if err != nil {
		return "", err
	}
	entries := listing.Files
	if isDir {
		entries = listing.Dirs
	}
	for _, item := range entries {
		if f.decodeName(item.Name) == leaf {
			return item.DocID, nil
		}
	}
	kind := "file"
	if isDir {
		kind = "directory"
	}
	return "", fmt.Errorf("moved PKU Disk %s %q could not be resolved in destination", kind, leaf)
}

func (o *Object) Fs() fs.Info { return o.fs }
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}
func (o *Object) Remote() string                    { return o.remote }
func (o *Object) ModTime(context.Context) time.Time { return o.modTime }
func (o *Object) Size() int64                       { return o.size }
func (o *Object) Storable() bool                    { return true }

func (o *Object) Hash(context.Context, hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

func (o *Object) SetModTime(context.Context, time.Time) error {
	return fs.ErrorCantSetModTime
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	meta := fileMetadata{
		DocID: o.id,
		Name:  o.fs.encodeName(path.Base(o.remote)),
		Size:  o.size,
		Rev:   o.rev,
	}
	if meta.Rev == "" || meta.Name == "" {
		fresh, err := o.fs.api.metadata(ctx, o.id)
		if err != nil {
			return nil, err
		}
		meta = fresh
	}
	signedURL, err := o.fs.api.downloadURL(ctx, meta)
	if err != nil {
		return nil, err
	}
	fs.FixRangeOption(options, o.size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return nil, err
	}
	for _, option := range options {
		key, value := option.Header()
		if key != "" {
			req.Header.Set(key, value)
		}
	}
	client, err := newObjectHTTPClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download from PKU object storage: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("download from PKU object storage: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, _ ...fs.OpenOption) error {
	leaf, parentID, err := o.fs.dirCache.FindPath(ctx, o.remote, true)
	if err != nil {
		return err
	}
	if parentID == virtualRootID {
		return errors.New("files cannot be uploaded directly into the PKU Disk virtual root")
	}
	meta, err := o.fs.api.upload(ctx, parentID, o.fs.encodeName(leaf), o.id, o.rev, src.Size(), src.ModTime(ctx), in)
	if err != nil {
		return err
	}
	o.id = meta.DocID
	o.size = meta.Size
	o.rev = meta.Rev
	o.modTime = fileModTime(meta.ClientMTime, meta.Modified)
	if o.modTime.IsZero() {
		o.modTime = src.ModTime(ctx)
	}
	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	if o.id == "" {
		return fs.ErrorObjectNotFound
	}
	return o.fs.api.deleteEntry(ctx, o.id, false)
}

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.OpenChunkWriter = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
)
