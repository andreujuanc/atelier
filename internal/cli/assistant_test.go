package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssistantArgs(t *testing.T) {
	root := setup(t)
	if code, _, errs := atl(t, "assistant", "a", "b"); code != exitUsage || !strings.Contains(errs, "accepts at most 1 arg") {
		t.Errorf("two args: %d %q", code, errs)
	}
	if code, _, errs := atl(t, "assistant", " "); code != exitUsage || !strings.Contains(errs, "empty task") {
		t.Errorf("empty task: %d %q", code, errs)
	}
	code, _, errs := atl(t, "assistant", "nope")
	if code != exitErr || !strings.Contains(errs, "nope") {
		t.Errorf("unknown task: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "atelier", "assistant")); err == nil {
		t.Error("assistant dir created for an unknown task")
	}
	if code, out, _ := atl(t, "--help"); code != 0 || !strings.Contains(out, "assistant") {
		t.Errorf("help lacks assistant: %d\n%s", code, out)
	}
}
