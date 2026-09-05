# rclone-pkudisk

`rclone-pkudisk` is an unofficial [rclone](https://rclone.org/) backend for Peking University PKU Disk (`disk.pku.edu.cn`), implemented against the AnyShare 7 APIs used by the service.

It builds a standalone rclone executable with the `pkudisk` backend included. A separate system `rclone` installation is not required.

> [!IMPORTANT]
> This project is not affiliated with or supported by Peking University or AISHU/AnyShare. It relies on service APIs that may change without notice.

## Features

The backend currently supports the operations needed for ordinary file transfer and local-to-PKU backup workflows:

- list document libraries, directories, and files
- create and remove directories, including native recursive purge of non-empty directory trees
- upload new files and update existing files as AnyShare revisions
- signed single uploads and multipart uploads, with rclone-native concurrent and cross-process resumable chunk uploads for large files
- downloads and ranged reads
- remove files
- server-side file copies plus file and directory moves
- bounded retry of transient timeout, HTTP 429, and HTTP 5xx failures on read-only metadata/listing requests
- reversible filename encoding for names AnyShare rejects or normalizes
- source modification times through AnyShare `client_mtime`
- OAuth login and refresh independent of the official desktop client

The backend has been exercised against a live PKU Disk account with large directory trees and repeated incremental `rclone copy` runs.

## Installation

### Prebuilt binaries

Prebuilt archives are published on the [GitHub Releases](https://github.com/rijuyuezhu/rclone-pkudisk/releases) page together with `SHA256SUMS`.

Release versions include the embedded rclone version. For example, `v1.74.4-pkudisk.1` means rclone v1.74.4 with PKU Disk downstream revision 1. Archive names follow rclone's OS/architecture naming and cover the same release targets except AIX, which is not supported by the LevelDB dependency used for optional `pkudist` authentication. The archives are portable CGO-free cross-builds; the matrix follows rclone's platform coverage, but macOS packages do not include rclone's optional macFUSE/`cmount` build profile.

### Install with Go

Go 1.25 or newer is required:

```bash
go install github.com/rijuyuezhu/rclone-pkudisk@latest
```

The binary is installed as `rclone-pkudisk` under `GOBIN`, or under `$(go env GOPATH)/bin` when `GOBIN` is unset. The upstream rclone `selfupdate` command is disabled in `rclone-pkudisk` so it cannot accidentally replace this binary with an official rclone build that lacks the `pkudisk` backend.

### Build from source

```bash
git clone https://github.com/rijuyuezhu/rclone-pkudisk.git
cd rclone-pkudisk
go build -o bin/rclone-pkudisk .
```

Optionally install it into your user PATH:

```bash
install -Dm755 bin/rclone-pkudisk ~/.local/bin/rclone-pkudisk
```

Check that the backend is available:

```bash
rclone-pkudisk help backend pkudisk
```

## Configuration

Run the normal rclone configuration UI:

```bash
rclone-pkudisk config
```

Create a new remote, for example `pku`, and select `pkudisk` as the storage type. The default service endpoint is `https://disk.pku.edu.cn`.

### OAuth — recommended

Use the default `auth = oauth` mode for normal use, especially scheduled or unattended jobs.

The backend dynamically registers an AnyShare OAuth client, opens the normal authorization flow, and stores the resulting OAuth state in the rclone configuration. Refreshing credentials does not depend on the official PKU Disk desktop client being open.

After configuration, verify access with:

```bash
rclone-pkudisk lsd pku:
```

### Reuse the official Linux client

`auth = pkudist` can reuse the signed-in official PKU Disk desktop client's token. On Linux the default Chromium Local Storage database is:

```text
~/.config/AnyShare/Local Storage/leveldb
```

The database is read only; `rclone-pkudisk` does not modify the official client's profile.

This mode is useful for temporary CLI access, but OAuth is a better choice for unattended jobs because token refresh in `pkudist` mode still depends on the official client having refreshed its own session.

### Explicit token

`auth = token` accepts an explicit access token. It is intended mainly for debugging and controlled environments.

## PKU Disk layout

The backend exposes a virtual root where each PKU Disk document library is a top-level directory:

```text
pku:
├── <document-library-1>/
└── <document-library-2>/
```

Document libraries themselves cannot be created or removed through the normal file API, and files cannot be uploaded directly into the virtual root. Select a document library first:

```bash
rclone-pkudisk lsd pku:
rclone-pkudisk ls 'pku:<document-library>/'
```

## Usage

`rclone-pkudisk` uses the normal rclone command-line interface.

List files:

```bash
rclone-pkudisk lsf 'pku:<document-library>/'
```

Copy a directory to PKU Disk:

```bash
rclone-pkudisk copy ~/Documents \
  'pku:<document-library>/Documents' \
  --progress
```

Download files:

```bash
rclone-pkudisk copy \
  'pku:<document-library>/Documents' \
  ~/Documents-from-PKU \
  --progress
```

Move or rename a file:

```bash
rclone-pkudisk moveto \
  'pku:<document-library>/old-name.txt' \
  'pku:<document-library>/new-name.txt'
```

Use `--dry-run` before an unfamiliar or potentially destructive operation:

```bash
rclone-pkudisk copy ~/Documents \
  'pku:<document-library>/Documents' \
  --dry-run -v
```

## Using PKU Disk as a backup target

For a one-way backup where the local filesystem is authoritative, prefer `copy`:

```bash
rclone-pkudisk copy ~/important-data \
  'pku:<document-library>/backup/important-data'
```

Repeated `copy` runs upload new or changed files but do **not** remove an existing remote file merely because it was deleted locally. This is generally the safer behavior for a backup target.

By contrast, `sync` makes the destination match the source and can delete destination files that no longer exist locally:

```bash
# Mirror semantics: destination-only files may be deleted.
rclone-pkudisk sync ~/mirror-me \
  'pku:<document-library>/mirror-me'
```

Use `sync` only when mirror semantics are actually intended. rclone's generic sync engine removes destination-only files individually before removing empty directories. If an entire remote subtree is known to be obsolete, `purge` is much more efficient because this backend maps it to AnyShare's native recursive directory deletion:

```bash
rclone-pkudisk purge 'pku:<document-library>/obsolete-tree'
```

### Symlinks

rclone skips symbolic links by default. To back up the contents pointed to by symlinks, use `-L` / `--copy-links`:

```bash
rclone-pkudisk copy -L ~/important-data \
  'pku:<document-library>/backup/important-data'
```

`--copy-links` follows both file and directory symlinks. Be careful with directory symlink cycles: rclone itself does not maintain a general visited-directory set, so callers that follow arbitrary directory symlinks should ensure the source tree does not contain recursive directory links or filter those links explicitly.

## Modification times

AnyShare exposes both a server-side update timestamp (`modified`) and a client-provided timestamp (`client_mtime`). `rclone-pkudisk` exposes `client_mtime` as rclone `ModTime`, falling back to `modified` when it is absent, and uploads the source modification time as `client_mtime`.

This matters because the APIs currently used by the backend do not expose a content hash. Ordinary rclone comparison therefore relies mainly on size plus modification time.

Changing the modification time of an already-uploaded object without uploading new content is not supported, so `SetModTime` returns rclone's `ErrorCantSetModTime`.

## Filename compatibility

AnyShare rejects or normalizes some names that are valid on Linux. The backend uses rclone's standard reversible `encoder.MultiEncoder` rather than a custom escaping scheme.

The default encoding covers Windows-style reserved characters, control characters, `.` and `..`, leading/trailing spaces, trailing periods, invalid UTF-8, and related edge cases. Names are decoded again when listed, so rclone callers continue to see their original names.

## Upload and download behavior

Uploads require a known object size. Small files use AnyShare's signed single-upload flow; larger files use its multipart flow while streaming the source rather than buffering the whole object in memory. When rclone selects its multi-thread copy path (256 MiB and above by default), the backend exposes AnyShare multipart parts through `OpenChunkWriter`, so rclone can upload independent parts concurrently. The usual rclone `--multi-thread-cutoff` and `--multi-thread-streams` controls determine when the multi-thread path is considered; the backend currently advertises four-way multipart concurrency.

Interrupted `OpenChunkWriter` uploads are resumable across rclone process restarts. Minimal state is kept under rclone's cache directory in `pkudisk/multipart`: the AnyShare upload ID, completed part ETags/sizes, source size+mtime, and the pre-existing destination ID/revision for updates. Signed object-storage URLs and authorization headers are never persisted. A changed source or destination revision invalidates the state; an expired upload ID is refreshed and restarted safely from part 1. Successful completion removes the JSON state. Platforms without advisory file locking keep the ordinary multipart behavior but disable cross-process resume.

Downloads use signed AnyShare object-storage URLs and support rclone range requests. The backend also bundles the TrustAsia intermediate certificate needed on systems where the PKU object-storage endpoint does not serve that intermediate in its certificate chain.

## Current limitations

Several optional rclone capabilities are not implemented yet:

- `ListR`
- content hashes
- quota / `About`
- change notifications
- changing mtime without uploading content
- uploads whose size is unknown in advance

The most visible performance limitation is the lack of `ListR`: scans of trees containing many directories require more API requests than a backend with recursive listing support.

Server-side file `Copy` is implemented for new destinations. If the destination already exists, the backend deliberately falls back to rclone's normal update path because AnyShare's copy-overwrite mode has no destination revision guard; this preserves the optimistic-concurrency protection used by ordinary updates.

## Development

Run the test suite and static checks with:

```bash
go test ./...
go vet ./...
go build -o bin/rclone-pkudisk .
```

The project intentionally embeds the backend into a small rclone executable rather than patching an installed rclone tree. The backend implementation lives under `backend/pkudisk`.

Release tags are derived from the pinned rclone version plus `REVISION`; `./scripts/version.sh release` prints the exact tag to create. Pushing that tag runs the release workflow, which tests the tree, builds the 28 targets listed in `scripts/release-targets.txt`, and publishes the archives with `SHA256SUMS`. A single archive can be built locally with `./scripts/build-release.sh linux/amd64`.

## License

MIT. See [LICENSE](LICENSE).
