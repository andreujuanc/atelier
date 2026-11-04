package palette

import (
	"testing"

	"github.com/andreujuanc/atelier/internal/model"
)

func TestANSI(t *testing.T) {
	for _, c := range []struct {
		st   model.SessionState
		dark bool
		want string
	}{
		{model.StateWaiting, true, "\x1b[1;38;2;247;118;142mx\x1b[0m"},
		{model.StateWaiting, false, "\x1b[1;38;2;181;23;158mx\x1b[0m"},
		{model.StateRunning, true, "\x1b[38;2;158;206;106mx\x1b[0m"},
		{model.StateIdle, true, "\x1b[38;2;102;102;102mx\x1b[0m"},
		{model.StateStopped, true, "\x1b[2;38;2;255;95;95mx\x1b[0m"},
		{model.StateEnded, true, "x"},
		{"", true, "x"},
	} {
		if got := StateLook(c.st).ANSI("x", c.dark); got != c.want {
			t.Errorf("%q dark=%v: %q, want %q", c.st, c.dark, got, c.want)
		}
	}
}

func TestDarkBackground(t *testing.T) {
	for v, want := range map[string]bool{
		"": true, "15;0": true, "0;15": false, "0;7": false, "0;default;15": false, "7;8": true, "x": true, "0;12": false,
	} {
		if got := DarkBackground(v); got != want {
			t.Errorf("COLORFGBG=%q: %v, want %v", v, got, want)
		}
	}
}
