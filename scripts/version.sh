#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
rclone_version=$(awk '$1 == "github.com/rclone/rclone" { print $2; exit }' "$root/go.mod")
revision=$(head -n 1 "$root/REVISION")

if [[ ! $rclone_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "invalid rclone version in go.mod: $rclone_version" >&2
  exit 1
fi
if [[ ! $revision =~ ^[1-9][0-9]*$ ]]; then
  echo "REVISION must be a positive integer: $revision" >&2
  exit 1
fi

suffix="pkudisk.${revision}"
release="${rclone_version}-${suffix}"

case "${1:-}" in
  rclone)
    printf '%s\n' "$rclone_version"
    ;;
  revision)
    printf '%s\n' "$revision"
    ;;
  suffix)
    printf '%s\n' "$suffix"
    ;;
  release|tag)
    printf '%s\n' "$release"
    ;;
  *)
    echo "usage: $0 {rclone|revision|suffix|release|tag}" >&2
    exit 2
    ;;
esac
