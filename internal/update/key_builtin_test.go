package update

import "testing"

// TestBuiltinReleaseKey: atl is built with exactly the Ed25519 keys in
// release-key.pub, and at least one: without it every update is refused.
func TestBuiltinReleaseKey(t *testing.T) {
	keys, err := ParseKeys(releaseKeyPEM)
	if err != nil || len(keys) == 0 {
		t.Fatalf("release-key.pub: %d keys, %v", len(keys), err)
	}
	if got := ReleaseKeys(); len(got) != len(keys) {
		t.Errorf("ReleaseKeys() = %d keys, want %d", len(got), len(keys))
	}
}
