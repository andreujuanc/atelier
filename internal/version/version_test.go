package version

import "testing"

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Semver
		ok   bool
	}{
		{"v0.1.0", Semver{0, 1, 0}, true},
		{"v1.22.333", Semver{1, 22, 333}, true},
		{"0.1.0", Semver{}, false},
		{"v0.1", Semver{}, false},
		{"v0.1.0-rc.1", Semver{}, false},
		{"v0.01.0", Semver{}, false},
		{"v0.+1.0", Semver{}, false},
		{"v0..0", Semver{}, false},
		{"dev", Semver{}, false},
		{"", Semver{}, false},
	} {
		got, ok := Parse(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("Parse(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		cur, latest string
		want        bool
	}{
		{"v0.1.4", "v0.1.5", true},
		{"v0.1.9", "v0.1.10", true},
		{"v0.1.10", "v0.2.0", true},
		{"v0.9.0", "v1.0.0", true},
		{"v0.1.5", "v0.1.5", false},
		{"v0.2.0", "v0.1.99", false},
		{Dev, "v0.1.5", false},
		{"v0.1.4", "garbage", false},
	} {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestCurrent(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = ""
	if Current() != Dev {
		t.Errorf("Current() = %q, want %q", Current(), Dev)
	}
	Version = "v0.3.1"
	if Current() != "v0.3.1" || String() != "v0.3.1" {
		t.Errorf("Current() = %q, String() = %q, want v0.3.1", Current(), String())
	}
}
