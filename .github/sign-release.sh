#!/bin/sh
# sign-release.sh KEY TAG SUMS SIG PUBKEYS
#
# Signs a release (release.yml): writes to SIG the Ed25519 signature, made
# with the private key in file KEY (PEM), of "atelier release TAG\n"
# followed by the file SUMS (internal/update.SignedMessage). Then checks
# the signature against the public keys in PUBKEYS (the
# internal/update/release-key.pub atl is built with) and fails unless one
# of them verifies it: a release atl would refuse is never published.
set -eu
[ $# -eq 5 ] || {
	echo "usage: sign-release.sh KEY TAG SUMS SIG PUBKEYS" >&2
	exit 2
}
key=$1 tag=$2 sums=$3 sig=$4 pubkeys=$5
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
{
	printf 'atelier release %s\n' "$tag"
	cat "$sums"
} > "$tmp/msg"
openssl pkeyutl -sign -inkey "$key" -rawin -in "$tmp/msg" -out "$sig"

# openssl reads one key per file: split PUBKEYS into its PEM blocks.
awk -v d="$tmp" '
	/-----BEGIN PUBLIC KEY-----/ { n++; f = d "/pub" n ".pem" }
	f { print > f }
	/-----END PUBLIC KEY-----/ { close(f); f = "" }
' "$pubkeys"
for pub in "$tmp"/pub*.pem; do
	[ -e "$pub" ] || break
	if openssl pkeyutl -verify -pubin -inkey "$pub" -rawin -in "$tmp/msg" -sigfile "$sig" > /dev/null 2>&1; then
		echo "signed $tag; the signature checks out with $(basename "$pubkeys")"
		exit 0
	fi
done
rm -f "$sig"
echo "sign-release.sh: the signing key matches no public key in $pubkeys: atl would refuse this release" >&2
exit 1
