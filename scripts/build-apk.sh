#!/usr/bin/env bash
set -euo pipefail

repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
package=luci-app-sms-archive
version=${SMS_ARCHIVE_VERSION:-0.1.0}
release=${SMS_ARCHIVE_RELEASE:-1}
key=${SMS_ARCHIVE_SIGNING_KEY:?Set SMS_ARCHIVE_SIGNING_KEY to the P-256 private PEM file}
key=$(realpath -- "$key")
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && $release =~ ^[1-9][0-9]*$ && -r $key ]]

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
target=https://downloads.openwrt.org/releases/25.12.6/targets/mediatek/filogic
sdk_archive=openwrt-sdk-25.12.6-mediatek-filogic_gcc-14.3.0_musl.Linux-x86_64.tar.zst
sdk_sha256=1c04ac47bdfc361d6aab299eb520d570e368013dedcb8d29cf39dc7168ae642e
kmods=$target/kmods/6.12.112-1-f0a7348f9cb959e1bf4d0447669b031e

download() {
    curl --fail --location --silent --show-error --retry 3 "$1" --output "$2"
}

if [[ -n ${SMS_ARCHIVE_SDK_DIR:-} ]]; then
    sdk=$(realpath -- "$SMS_ARCHIVE_SDK_DIR")
else
    download "$target/$sdk_archive" "$work/$sdk_archive"
    printf '%s  %s\n' "$sdk_sha256" "$work/$sdk_archive" | sha256sum -c -
    tar --zstd -xf "$work/$sdk_archive" -C "$work"
    sdk="$work/${sdk_archive%.tar.zst}"
fi
host_apk=$sdk/staging_dir/host/bin/apk
[[ -x $host_apk && -f $sdk/rules.mk ]]

staged=$sdk/package/$package
mkdir -p "$staged"
cp -- "$repo/package/$package/Makefile" "$repo/package/$package/uci-defaults" "$repo/LICENSE" "$staged/"
mkdir -p "$staged/files"
rsync -r --delete "$repo/files/" "$staged/files/"

(
    cd "$repo"
    export CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOARM64=v8.0
    go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$staged/sms-archive" ./cmd/sms-archive
    {
        printf 'Go standard library\n\n'
        cat "$(go env GOROOT)/LICENSE" "$(go env GOROOT)/PATENTS"
        go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/sms-archive |
            sort -u | while read -r module module_version path; do
                [[ -n $module ]] || continue
                printf '\n\n%s %s\n\n' "$module" "$module_version"
                shopt -s nullglob
                notices=("$path"/LICENSE* "$path"/COPYING* "$path"/NOTICE* "$path"/PATENTS*)
                if (( ${#notices[@]} == 0 )); then
                    printf 'Missing license notice: %s\n' "$module" >&2
                    exit 1
                fi
                cat "${notices[@]}"
            done
    } > "$staged/THIRD-PARTY-LICENSES"
)

(
    cd "$sdk"
    export SMS_ARCHIVE_VERSION=$version SMS_ARCHIVE_RELEASE=$release
    printf '\nCONFIG_PACKAGE_%s=m\n' "$package" >> .config
    make defconfig
    make "package/$package/clean"
    make "package/$package/compile"
)

name=$package-$version-r$release.apk
built=$(find "$sdk/bin" -type f -name "$name" -print -quit)
[[ -n $built ]]

feed=$work/feed
offline=$work/offline
mkdir -p "$feed" "$offline/kmods"
cp -- "$built" "$feed/"
openssl pkey -in "$key" -pubout -out "$feed/sms-archive.pub.pem"
"$host_apk" mkndx --allow-untrusted --sign "$key" --output "$feed/packages.adb" "$feed/$name"
cp -- "$feed/"* "$offline/"

# The stock Cudy image already provides the LuCI libraries and USB core.
download "$kmods/packages.adb" "$offline/kmods/packages.adb"
download "$kmods/kmod-usb-acm-6.12.112-r1.apk" "$offline/kmods/kmod-usb-acm-6.12.112-r1.apk"

out=${SMS_ARCHIVE_OUTPUT:-$repo/dist}
mkdir -p "$out/feed" "$out/release"
cp -- "$feed/"* "$out/feed/"
cp -- "$feed/"* "$out/release/"
bundle=$package-$version-r$release-offline.tar.gz
tar -czf "$out/release/$bundle" -C "$offline" .
(cd "$out/release" && sha256sum "$name" "$bundle" packages.adb sms-archive.pub.pem > SHA256SUMS)
printf 'Built %s and %s\n' "$name" "$bundle"
