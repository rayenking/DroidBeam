#!/usr/bin/env bash
# Build release artifacts into release/:
#   - droidbeam-server for Linux, macOS and Windows (archives)
#   - the signed Android APK (needs android/keystore.properties)
#   - SHA256SUMS
#
#   ./scripts/build-release.sh 0.1.0
set -euo pipefail

version="${1:?usage: $0 <version, e.g. 0.1.0>}"
version="${version#v}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/release"
rm -rf "$out" && mkdir -p "$out"

targets=(linux/amd64 linux/arm64 linux/arm/7 darwin/amd64 darwin/arm64 windows/amd64)

echo "==> server tests"
(cd "$root/server" && go test ./...)

for t in "${targets[@]}"; do
  IFS=/ read -r goos goarch goarm <<<"$t"
  arch="$goarch${goarm:+v$goarm}"
  name="droidbeam-server_${version}_${goos}_${arch}"
  exe="droidbeam-server"; [[ "$goos" == windows ]] && exe="droidbeam-server.exe"
  echo "==> $name"
  stage="$(mktemp -d)"
  (cd "$root/server" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" \
    go build -trimpath -tags timetzdata -ldflags "-s -w -X main.version=$version" -o "$stage/$exe" .)
  cp "$root/README.md" "$stage/"
  if [[ "$goos" == windows ]]; then
    (cd "$stage" && zip -q "$out/$name.zip" "$exe" README.md)
  else
    tar -C "$stage" -czf "$out/$name.tar.gz" "$exe" README.md
  fi
  rm -rf "$stage"
done

echo "==> android release APK"
[[ -f "$root/android/keystore.properties" ]] || echo "warning: android/keystore.properties missing, APK will be unsigned" >&2
(cd "$root/android" && ./gradlew -q assembleRelease)
apk="$root/android/app/build/outputs/apk/release/app-release.apk"
[[ -f "$apk" ]] || apk="$root/android/app/build/outputs/apk/release/app-release-unsigned.apk"
cp "$apk" "$out/DroidBeam-v$version.apk"

(cd "$out" && shasum -a 256 * > SHA256SUMS)
echo "==> done"
ls -lh "$out"
