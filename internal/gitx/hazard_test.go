package gitx

import (
	"context"
	"path/filepath"
	"testing"
)

// TestHazardScanReadsWhatTheCommandReads: the scan runs with the command's
// -c options, so a conditional include that depends on them is followed as
// the command follows it.
func TestHazardScanReadsWhatTheCommandReads(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	included := filepath.Join(t.TempDir(), "work.cfg")
	writeFile(t, included, "[http]\n\tproxy = http://proxy.example:1\n")
	global := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, global, "[includeIf \"hasconfig:remote.*.url:https://h/o/r.git\"]\n\tpath = "+included+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	seen := func(cfg []string) bool {
		entries, err := scanConfig(ctx, f.repo, `^http\.proxy$`, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries) == 1 && entries[0].scope == "global"
	}
	if seen(nil) {
		t.Error("the include was followed without a remote that has its URL")
	}
	if !seen([]string{"-c", "remote.atl-x.url=https://h/o/r.git"}) {
		t.Error("the scan did not follow the include the command's -c options make true")
	}
}
