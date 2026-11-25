// Package version is atl's version: the release tag (e.g. "v0.1.4") in a
// release build, which sets Version with
//
//	-ldflags "-X github.com/andreujuanc/atelier/internal/version.Version=v0.1.4"
//
// and "dev" in any other build.
package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// Version is set by the release build; empty in every other build.
var Version = ""

// Dev is the version of a build that is not a release.
const Dev = "dev"

// Current is the running atl's version: Version, else Dev.
func Current() string {
	if Version != "" {
		return Version
	}
	return Dev
}

// String is Current with, for a dev build, the commit it was built from
// when Go recorded one (go build in a git checkout): "dev (1a2b3c4d5e6f,
// modified)".
func String() string {
	v := Current()
	if v != Dev {
		return v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return v
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += ", modified"
	}
	return v + " (" + rev + ")"
}

// Semver is a release version vMAJOR.MINOR.PATCH.
type Semver struct{ Major, Minor, Patch int }

// Parse parses "vMAJOR.MINOR.PATCH" (no pre-release or build suffix: atl
// releases have none). ok is false for anything else, Dev included.
func Parse(s string) (v Semver, ok bool) {
	rest, found := strings.CutPrefix(s, "v")
	if !found {
		return v, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return v, false
	}
	var n [3]int
	for i, p := range parts {
		// Digits only, no leading zero (strconv.Atoi takes "+1" and "01").
		if p == "" || (len(p) > 1 && p[0] == '0') || strings.TrimLeft(p, "0123456789") != "" {
			return v, false
		}
		x, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		n[i] = x
	}
	return Semver{n[0], n[1], n[2]}, true
}

// Less reports whether v is older than w.
func (v Semver) Less(w Semver) bool {
	if v.Major != w.Major {
		return v.Major < w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor < w.Minor
	}
	return v.Patch < w.Patch
}

// Newer reports whether release latest is newer than current. A current
// that is not a release (Dev) is never older: atl does not offer to
// replace a local build. An unparsable latest is never newer.
func Newer(current, latest string) bool {
	c, ok1 := Parse(current)
	l, ok2 := Parse(latest)
	return ok1 && ok2 && c.Less(l)
}
