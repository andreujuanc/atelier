package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestParseBaseFlag(t *testing.T) {
	for in, want := range map[string]string{
		"":             model.BaseAuto,
		"auto":         model.BaseAuto,
		" auto ":       model.BaseAuto,
		"default":      model.BaseDefault,
		"production":   model.BaseDefault,
		":default":     model.BaseDefault,
		"main":         "main",
		"dev":          "dev",
		"release/1.2":  "release/1.2",
		" hotfix/x  ":  "hotfix/x",
		"Default-ish":  "Default-ish",
		"develop":      "develop",
		"prod-support": "prod-support",
	} {
		if got, err := parseBaseFlag(in); err != nil || got != want {
			t.Errorf("parseBaseFlag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{":x", "origin/dev", "-f", "a b"} {
		if _, err := parseBaseFlag(bad); err == nil {
			t.Errorf("parseBaseFlag(%q) succeeded", bad)
		}
	}
}

// showBase returns the task's "base" and each repo's "base_branch" from
// `atl show --json`.
func showBase(t *testing.T, slug string) (string, map[string]string) {
	t.Helper()
	code, out, errs := atl(t, "show", slug, "--json")
	if code != 0 {
		t.Fatalf("show %s: %d %s", slug, code, errs)
	}
	var d struct {
		Base  string `json:"base"`
		Repos []struct {
			Name       string `json:"name"`
			BaseBranch string `json:"base_branch"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, r := range d.Repos {
		m[r.Name] = r.BaseBranch
	}
	return d.Base, m
}

func TestNewBaseFlag(t *testing.T) {
	root := setup(t)
	seed := filepath.Join(root, "seed")
	// origin gets a dev branch one commit ahead of main, after the clone.
	git(t, seed, "checkout", "-q", "-b", "dev")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "dev work")
	git(t, seed, "push", "-q", filepath.Join(root, "origin.git"), "dev")
	git(t, filepath.Join(root, "repos"), "clone", "-q", filepath.Join(root, "origin.git"), "web")

	if code, _, errs := atl(t, "new", "feature", "--repo", "api"); code != 0 {
		t.Fatalf("new auto: %d %s", code, errs)
	}
	if base, repos := showBase(t, "feature"); base != "" || repos["api"] != "dev" {
		t.Fatalf("auto: base %q repos %v", base, repos)
	}
	if code, out, _ := atl(t, "show", "feature"); code != 0 || !strings.Contains(out, "base origin/dev") {
		t.Fatalf("show text:\n%s", out)
	}
	if code, _, errs := atl(t, "new", "hotfix", "--repo", "api", "--base", "production"); code != 0 {
		t.Fatalf("new production: %d %s", code, errs)
	}
	if base, repos := showBase(t, "hotfix"); base != model.BaseDefault || repos["api"] != "main" {
		t.Fatalf("production: base %q repos %v", base, repos)
	}
	if code, out, _ := atl(t, "show", "hotfix"); code != 0 || !strings.Contains(out, "  base         default\n") ||
		!strings.Contains(out, "base origin/main") {
		t.Fatalf("show text:\n%s", out)
	}
	if code, _, errs := atl(t, "new", "on main", "--repo", "api", "--base=main", "--no-fetch"); code != 0 {
		t.Fatalf("new main: %d %s", code, errs)
	}
	if base, repos := showBase(t, "on-main"); base != "main" || repos["api"] != "main" {
		t.Fatalf("main: base %q repos %v", base, repos)
	}

	// atl add uses the task's stored base.
	if code, _, errs := atl(t, "add", "hotfix", "web"); code != 0 {
		t.Fatalf("add: %d %s", code, errs)
	}
	if _, repos := showBase(t, "hotfix"); repos["web"] != "main" {
		t.Fatalf("hotfix web %v", repos)
	}
	if code, _, errs := atl(t, "add", "feature", "web"); code != 0 {
		t.Fatalf("add: %d %s", code, errs)
	}
	if _, repos := showBase(t, "feature"); repos["web"] != "dev" {
		t.Fatalf("feature web %v", repos)
	}

	if code, _, errs := atl(t, "new", "x", "--repo", "api", "--base", "nope"); code != 1 ||
		!strings.Contains(errs, "repo api") || !strings.Contains(errs, `"nope"`) {
		t.Fatalf("missing branch: %d %s", code, errs)
	}
	if code, _, _ := atl(t, "new", "x", "--repo", "api", "--base", ":weird"); code != 2 {
		t.Fatalf("invalid base: exit %d", code)
	}
}
