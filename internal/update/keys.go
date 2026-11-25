package update

import (
	"crypto/ed25519"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
)

// releaseKeyPEM holds the public keys of the release key (PEM "PUBLIC
// KEY" blocks, as `openssl pkey -pubout` writes them; other text is
// ignored). During a key rotation it holds the old and the new key, so
// that atl built with either installs releases signed with the other.
// release.yml checks each signature it makes against this file.
//
//go:embed release-key.pub
var releaseKeyPEM []byte

// ReleaseKeys are the public keys built into atl. A file that does not
// parse yields none: Install then refuses every release.
func ReleaseKeys() []ed25519.PublicKey {
	keys, err := ParseKeys(releaseKeyPEM)
	if err != nil {
		return nil
	}
	return keys
}

// ParseKeys returns the Ed25519 public keys of the PEM "PUBLIC KEY"
// blocks in data.
func ParseKeys(data []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return keys, nil
		}
		if b.Type != "PUBLIC KEY" {
			continue
		}
		k, err := x509.ParsePKIXPublicKey(b.Bytes)
		if err != nil {
			return nil, err
		}
		ed, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("release key: %T, want an Ed25519 key", k)
		}
		keys = append(keys, ed)
	}
}

// SignedMessage is what a release's SHA256SUMS.sig signs: the tag, then
// SHA256SUMS. Naming the tag keeps a signature from vouching for another
// release (an older one, republished as newer).
func SignedMessage(tag string, sums []byte) []byte {
	return append([]byte("atelier release "+tag+"\n"), sums...)
}

// VerifySums checks that sig is an Ed25519 signature of SignedMessage(tag,
// sums) by one of keys.
func VerifySums(keys []ed25519.PublicKey, tag string, sums, sig []byte) error {
	if len(keys) == 0 {
		return errors.New("this atl has no release key to check releases with (a local build without internal/update/release-key.pub?)")
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%s is %d bytes, not an Ed25519 signature", SigAsset, len(sig))
	}
	msg := SignedMessage(tag, sums)
	for _, k := range keys {
		if ed25519.Verify(k, msg, sig) {
			return nil
		}
	}
	return fmt.Errorf("%s does not check out with atl's release key: not made by atelier's release workflow", SigAsset)
}
