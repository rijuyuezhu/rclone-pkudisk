#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 GOOS/GOARCH [OUTPUT_DIR]" >&2
  exit 2
fi

target=$1
output_dir=${2:-dist}
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

if ! grep -Fxq -- "$target" "$root/scripts/release-targets.txt"; then
  echo "unsupported release target: $target" >&2
  exit 1
fi

release=$("$root/scripts/version.sh" release)
suffix=$("$root/scripts/version.sh" suffix)
goos=${target%%/*}
arch_label=${target#*/}
goarch=$arch_label
package_os=$goos
extra_env=()
tags=(noselfupdate)

case "$arch_label" in
  386)
    extra_env+=(GO386=softfloat)
    ;;
  arm)
    goarch=arm
    extra_env+=(GOARM=5)
    ;;
  arm-v6)
    goarch=arm
    extra_env+=(GOARM=6)
    ;;
  arm-v7)
    goarch=arm
    extra_env+=(GOARM=7)
    ;;
  mips|mipsle)
    extra_env+=(GOMIPS=softfloat)
    ;;
esac

if [[ $goos == darwin ]]; then
  package_os=osx
fi
if [[ $goos == windows ]]; then
  tags+=(cmount)
fi

package="rclone-pkudisk-${release}-${package_os}-${arch_label}"
stage=$(mktemp -d "${TMPDIR:-/tmp}/rclone-pkudisk-release-XXXXXX")
trap 'rm -rf -- "$stage"' EXIT
package_dir="$stage/$package"
mkdir -p "$package_dir" "$output_dir"
output_dir=$(cd -- "$output_dir" && pwd)

binary="$package_dir/rclone-pkudisk"
if [[ $goos == windows ]]; then
  binary+=".exe"
fi

(
  cd "$root"
  env \
    CGO_ENABLED=0 \
    GOOS="$goos" \
    GOARCH="$goarch" \
    "${extra_env[@]}" \
    go build \
      -buildvcs=false \
      -trimpath \
      -tags "${tags[*]}" \
      -ldflags "-s -w -X github.com/rclone/rclone/fs.VersionSuffix=${suffix}" \
      -o "$binary" \
      .
)

cp "$root/README.md" "$root/LICENSE" "$package_dir/"
source_commit=$(git -C "$root" rev-parse HEAD)
source_epoch=${SOURCE_DATE_EPOCH:-$(git -C "$root" show -s --format=%ct HEAD)}
{
  printf 'release: %s\n' "$release"
  printf 'target: %s\n' "$target"
  printf 'rclone: %s\n' "$("$root/scripts/version.sh" rclone)"
  printf 'source-commit: %s\n' "$source_commit"
  printf 'build-tags: %s\n' "${tags[*]}"
  printf 'go: %s\n' "$(go version)"
  if module_info=$(go version -m "$binary" 2>/dev/null); then
    printf '\n'
    printf '%s\n' "$module_info" | sed '1s|^[^:]*:|binary:|'
  fi
} > "$package_dir/BUILDINFO.txt"
find "$package_dir" -exec touch -h -d "@$source_epoch" {} +

archive="$output_dir/$package.zip"
(
  cd "$stage"
  zip -X -9 -q -r "$archive" "$package"
)
printf '%s\n' "$archive"
