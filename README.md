# rclone-pkudisk

`rclone-pkudisk` is an out-of-tree [rclone](https://rclone.org/) backend for Peking University PKU Disk, backed by the AnyShare 7 APIs used by `disk.pku.edu.cn`.

The repository builds a small rclone executable with the `pkudisk` backend included. It is intended for normal rclone CLI usage and for long-running sync frontends such as Mt Sync.

## Status

The backend currently supports the operations needed for ordinary filesystem synchronization:

- list document libraries, directories, and files
- create and remove directories
- upload new files and update existing files as AnyShare revisions
- small signed uploads and multipart uploads
- download and range reads
- remove files
- server-side file moves and directory moves
- reversible filename encoding for names AnyShare rejects or normalizes
- microsecond file modification times through AnyShare `client_mtime`
- OAuth token refresh

It has been exercised against a live PKU Disk account with rclone's backend contracts for filename encoding, overwrite/update, range reads, file moves, rooted directory moves, empty files, removal, and common error paths.

## Build

Requirements:

- Go 1.25 or newer

Build the executable:

```bash
go build -o bin/rclone-pkudisk .
```

Check the backend is present:

```bash
./bin/rclone-pkudisk help backend pkudisk
```

Run the local test suite:

```bash
go test ./...
go vet ./...
```

## Configure

Run the normal rclone configuration UI:

```bash
./bin/rclone-pkudisk config
```

Create a remote and select `pkudisk` as its storage type.

### OAuth — recommended

Use `auth = oauth` for Mt Sync, mounts, scheduled synchronization, and other long-running jobs.

The backend dynamically registers an AnyShare OAuth client and stores the resulting rclone OAuth configuration. Access-token refresh is then independent of the official PKU Disk desktop client.

### Reuse the official desktop client

`auth = pkudist` reads the signed-in AnyShare desktop client's Chromium LevelDB without modifying it. On Linux the default location is:

```text
~/.config/AnyShare/Local Storage/leveldb
```

The backend understands Chromium LevelDB WAL records as well as table files, because the latest OAuth token may exist only in the active WAL.

This mode is useful for temporary CLI access, but it is not recommended for a long-running sync process: if the desktop client itself has not refreshed an expired token yet, rereading its profile still returns that expired token.

### Explicit token

`auth = token` accepts an explicit access token. It is mainly useful for debugging and controlled environments.

## Remote layout

The backend exposes a virtual root. Each PKU Disk document library appears as a top-level directory:

```text
remote:
├── <document-library-1>/
└── <document-library-2>/
```

Document libraries themselves cannot be created or deleted through the file API, and files cannot be uploaded directly into the virtual root.

Typical commands therefore target a document library or a directory below it:

```bash
./bin/rclone-pkudisk lsd pku:
./bin/rclone-pkudisk ls 'pku:<document-library>/'
./bin/rclone-pkudisk copy ./local-dir 'pku:<document-library>/backup'
./bin/rclone-pkudisk sync ./local-dir 'pku:<document-library>/sync-root'
```

For Mt Sync, use an OAuth-configured remote and choose a root inside one document library rather than the virtual root.

## Modification times

AnyShare exposes two different timestamps for files:

- `modified`: the server-side commit/update time
- `client_mtime`: the original client-provided file modification time

`rclone-pkudisk` uses `client_mtime` for rclone `ModTime`, falling back to `modified` only when `client_mtime` is absent. Uploads send the source modification time as `client_mtime`.

PKU Disk preserves this value at microsecond precision, including across server-side moves. This is important because PKU Disk does not expose a content hash through the APIs currently used by this backend; ordinary rclone comparison therefore relies on size plus modification time.

Changing the modification time of an already-uploaded object without uploading new content is not supported, so `SetModTime` returns rclone's `ErrorCantSetModTime`.

## Filename compatibility

AnyShare rejects or normalizes some names that are valid on Linux, including Windows-style reserved characters and leading/trailing whitespace.

The backend uses rclone's standard reversible `encoder.MultiEncoder` rather than a custom escaping scheme. The default encoding covers, among other cases:

- `\\ / : * ? " < > |`
- control characters and DEL
- `.` and `..`
- leading and trailing spaces
- trailing periods
- invalid UTF-8

Names are encoded only at the AnyShare boundary and decoded when listed, so rclone and sync frontends continue to see the original names.

The encoding can be overridden with the normal `encoding` advanced backend option.

## Upload and download behavior

Uploads require a known object size. This matches normal local-file synchronization and Mt Sync usage.

Small files use AnyShare's signed single-upload flow. Larger files use the multipart flow (`osinitmultiupload`, signed part uploads, completion, and `osendupload`) while streaming data rather than buffering the entire object in memory.

Downloads use signed AnyShare object-storage URLs and support rclone range requests. The bundled TrustAsia intermediate certificate compensates for the PKU object-storage endpoint omitting that intermediate from its served certificate chain on affected systems.

Object transfers do not have a backend-specific whole-transfer wall-clock timeout; cancellation and higher-level timeouts are left to rclone/context handling.

## Move semantics

AnyShare GNS document IDs can change when an entry changes parent. The backend therefore re-resolves the entry after a parent move before issuing any following operation.

When a move also renames an entry, the backend selects the safe server-side operation order based on intermediate-name conflicts:

- move then rename when the source name is free in the destination
- rename then move when moving first would collide
- return rclone's `ErrorCantMove` / `ErrorCantDirMove` when both possible intermediate names collide, allowing higher-level fallback instead of risking a partially mutated object

Directory moves use rclone's `dircache.DirMove` preparation logic, including rooted-Fs moves where `srcRemote` or `dstRemote` is empty.

## Current limitations

The backend intentionally does not yet implement several optional rclone capabilities:

- `ListR`
- server-side `Copy`
- content hashes
- quota / `About`
- change notifications
- changing mtime without a content upload
- uploads whose size is unknown in advance

These are not required for ordinary local-to-PKU synchronization. `ListR` is the most obvious future performance improvement for trees with many directories; server-side `Copy` would avoid download/re-upload for PKU-to-PKU copies.

## Authentication note for long-running sync

For a desktop synchronization setup, the recommended stack is:

```text
Mt Sync
   ↓
rclone-pkudisk
   ↓  OAuth
PKU Disk / AnyShare
```

Use `auth = oauth` for this path. Treat `auth = pkudist` as a convenience mode for reusing a currently healthy official-client session, not as the durable credential source for an unattended synchronizer.

## License

MIT. See [COPYING](COPYING).
