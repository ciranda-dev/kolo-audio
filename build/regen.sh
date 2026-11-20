#!/bin/sh
# Regenerates the Go packages from the committed wasm files.
# Usage: regen.sh [rootdir [target...]]   (no target means all of them)
# wasm2go is pinned in tools/go.mod. It needs a warm module cache or the network.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=${1:-$(dirname "$here")}
[ $# -gt 0 ] && shift
want=${*:-all}
known=
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# gen name package wasm output
gen() {
	known="$known $1"
	case " $want " in *" all "* | *" $1 "*) ;; *) return 0 ;; esac
	(cd "$here/tools" && GOFLAGS= GOWORK=off go tool wasm2go -unsafe -pkg "$2" -o "$tmp/$1.go" "$here/$3")
	head -n 1 "$tmp/$1.go" | grep -Eq '^// Code generated .* DO NOT EDIT\.$' ||
		{ echo "regen.sh: $4 has no generated-code header" >&2; exit 1; }
	[ -z "$(gofmt -l "$tmp/$1.go")" ] || { echo "regen.sh: $4 is not gofmt-clean" >&2; exit 1; }
	mkdir -p "$(dirname "$root/$4")"
	if cmp -s "$tmp/$1.go" "$root/$4"; then echo "$4: unchanged"; return 0; fi
	cp "$tmp/$1.go" "$root/$4.new"
	mv "$root/$4.new" "$root/$4"
	echo "$4: written"
}

gen libopus libopus libopus/libopus.wasm opus/internal/libopus/libopus.go
gen maresample maresample maresample/maresample.wasm internal/maresample/resample.go

for t in $want; do
	[ "$t" = all ] && continue
	case " $known " in *" $t "*) ;; *) echo "regen.sh: unknown target $t" >&2; exit 1 ;; esac
done
