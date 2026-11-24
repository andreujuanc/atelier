package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
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

// TestHazardTextIsPrintable: a task writes the repo's config, the names of
// sections included, and atl prints a hazard's key and value (warnings,
// errors, atl doctor, the TUI): its control characters (a terminal's
// escape sequences) are shown escaped, never sent as they are.
func TestHazardTextIsPrintable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f2, err := os.OpenFile(filepath.Join(f.repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f2, "[url \"https://evil/\x1b]52;c;ZWNobyBwd25lZA==\x07\"]\n\tinsteadOf = a\x1b[2J\u202eb\n")
	f2.Close()
	hs, err := Hazards(ctx, f.repo, nil)
	if err != nil || len(hs) != 1 {
		t.Fatalf("hazards = %v, %v", hs, err)
	}
	h := hs[0]
	for _, s := range []string{h.Key, h.Value, h.String(), (&RefusedError{Dir: f.repo, Hazard: h}).Error()} {
		if i := strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }); i >= 0 {
			t.Errorf("%q has a control character", s)
		}
	}
	if want := `url.https://evil/\x1b]52;c;ZWNobyBwd25lZA==\a.insteadof = a\x1b[2J\u202eb`; h.Key+" = "+h.Value != want {
		t.Errorf("hazard %s = %s, want %s", h.Key, h.Value, want)
	}
}
