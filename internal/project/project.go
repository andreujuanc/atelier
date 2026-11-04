// Package project loads project definitions: named groups of repos with
// optional setup/teardown scripts, from ConfigDir/projects/<name>.toml.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/andreujuanc/atelier/internal/config"
	"github.com/andreujuanc/atelier/internal/model"
)

// RepoSpec is one repo of a project with its default mode (rw if omitted).
type RepoSpec struct {
	Name string     `toml:"name"`
	Mode model.Mode `toml:"mode"`
}

// Project is a parsed project file.
type Project struct {
	Name     string            `toml:"-"`
	Repos    []RepoSpec        `toml:"repos"`
	Setup    string            `toml:"setup"`    // sh -c in each rw worktree after creation
	Teardown string            `toml:"teardown"` // sh -c in each rw worktree before removal
	Env      map[string]string `toml:"env"`
}

// Load reads and validates the project called name. An unknown name yields
// an error listing the available projects.
func Load(p config.Paths, name string) (*Project, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("project %q: %w", name, err)
	}
	path := p.ProjectFile(name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		names, lerr := List(p)
		if lerr != nil {
			return nil, fmt.Errorf("project %q not found (%s): listing projects: %w", name, path, lerr)
		}
		avail := "none"
		if len(names) > 0 {
			avail = strings.Join(names, ", ")
		}
		return nil, fmt.Errorf("project %q not found (no %s); available: %s", name, path, avail)
	}
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", name, err)
	}
	var pr Project
	md, err := toml.Decode(string(data), &pr)
	if err != nil {
		return nil, fmt.Errorf("project %q: parse %s: %w", name, path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("project %q: %s: unknown keys: %s", name, path, strings.Join(keys, ", "))
	}
	pr.Name = name
	seen := map[string]bool{}
	for i := range pr.Repos {
		r := &pr.Repos[i]
		r.Name = strings.TrimSpace(r.Name)
		if err := validName(r.Name); err != nil {
			return nil, fmt.Errorf("project %q: repo #%d: %w", name, i+1, err)
		}
		if r.Mode == "" {
			r.Mode = model.ModeRW
		}
		if !r.Mode.Valid() {
			return nil, fmt.Errorf("project %q: repo %q: invalid mode %q (want rw or ro)", name, r.Name, r.Mode)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("project %q: repo %q listed twice", name, r.Name)
		}
		seen[r.Name] = true
	}
	return &pr, nil
}

// validName checks a project or repo name: non-empty, a single path
// component.
func validName(n string) error {
	switch {
	case n == "":
		return errors.New("empty name")
	case strings.ContainsAny(n, `/\`) || n == "." || n == "..":
		return fmt.Errorf("invalid name %q (must not contain path separators)", n)
	}
	return nil
}

// List returns the sorted names of all projects (*.toml files in
// ConfigDir/projects). A missing directory yields an empty list.
func List(p config.Paths) ([]string, error) {
	dir := filepath.Join(p.ConfigDir, "projects")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		if n := strings.TrimSuffix(e.Name(), ".toml"); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}
