#!/bin/sh
# Downloads the RFC 8251 Opus test vectors, checks the pinned sha256 and extracts them.
#
#   vectors.sh <dir>      <dir> must not exist or be empty; prints the vector directory (for KOLO_OPUS_VECTORS)
set -eu

URL=https://opus-codec.org/static/testvectors/opus_testvectors-rfc8251.tar.gz
SHA256=6b26a22f9ba87b2b836906a9bb7afec5f8e54d49553b1200382520ee6fedfa55

dir=${1:?usage: vectors.sh <dir>}
mkdir -p "$dir"
[ -z "$(ls -A "$dir")" ] || { echo "vectors.sh: $dir is not empty" >&2; exit 1; }
curl -fsSL -o "$dir/vectors.tar.gz" "$URL"
got=$(if command -v sha256sum >/dev/null; then sha256sum "$dir/vectors.tar.gz"; else shasum -a 256 "$dir/vectors.tar.gz"; fi | cut -d' ' -f1)
[ "$got" = "$SHA256" ] || { echo "vectors.sh: checksum mismatch: $got" >&2; exit 1; }
tar xzf "$dir/vectors.tar.gz" -C "$dir"
rm "$dir/vectors.tar.gz"
echo "$dir/opus_newvectors"
