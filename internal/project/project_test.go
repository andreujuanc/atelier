package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
)

func paths(t *testing.T) config.Paths {
	t.Helper()
	return config.Paths{ConfigDir: filepath.Join(t.TempDir(), "atelier")}
}

func write(t *testing.T, p config.Paths, name, body string) {
	t.Helper()
	f := p.ProjectFile(name)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadValid(t *testing.T) {
	p := paths(t)
	write(t, p, "shop", `
setup = "make dev-setup"
teardown = "docker compose down -v"
[[repos]]
name = "api"
mode = "rw"
[[repos]]
name = "design-system"
mode = "ro"
[[repos]]
name = "web"
[env]
COMPOSE_PROJECT_NAME_PREFIX = "shop"
`)
	pr, err := Load(p, "shop")
	if err != nil {
		t.Fatal(err)
	}
	want := &Project{
		Name: "shop",
		Repos: []RepoSpec{
			{Name: "api", Mode: model.ModeRW},
			{Name: "design-system", Mode: model.ModeRO},
			{Name: "web", Mode: model.ModeRW}, // default mode
		},
		Setup:    "make dev-setup",
		Teardown: "docker compose down -v",
		Env:      map[string]string{"COMPOSE_PROJECT_NAME_PREFIX": "shop"},
	}
	if !reflect.DeepEqual(pr, want) {
		t.Fatalf("got %+v\nwant %+v", pr, want)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := map[string]string{
		"syntax":    `[[repos]` + "\n",
		"emptyname": "[[repos]]\nname = \"\"\n",
		"slash":     "[[repos]]\nname = \"a/b\"\n",
		"dotdot":    "[[repos]]\nname = \"..\"\n",
		"mode":      "[[repos]]\nname = \"a\"\nmode = \"rx\"\n",
		"dup":       "[[repos]]\nname = \"a\"\n[[repos]]\nname = \"a\"\nmode = \"ro\"\n",
		"unknown":   "setpu = \"x\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := paths(t)
			write(t, p, "x", body)
			if pr, err := Load(p, "x"); err == nil {
				t.Fatalf("expected error, got %+v", pr)
			}
		})
	}
}

func TestLoadUnknown(t *testing.T) {
	p := paths(t)
	if _, err := Load(p, "nope"); err == nil || !strings.Contains(err.Error(), "available: none") {
		t.Fatalf("err = %v", err)
	}
	write(t, p, "b", "")
	write(t, p, "a", "")
	_, err := Load(p, "nope")
	if err == nil || !strings.Contains(err.Error(), "available: a, b") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Load(p, "../a"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestList(t *testing.T) {
	p := paths(t)
	names, err := List(p)
	if err != nil || len(names) != 0 {
		t.Fatalf("missing dir: %v %v", names, err)
	}
	write(t, p, "zeta", "")
	write(t, p, "alpha", "")
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "projects", "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(p.ConfigDir, "projects", "dir.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	names, err = List(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Fatalf("names = %v", names)
	}
}
