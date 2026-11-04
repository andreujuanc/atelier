package tmux

import (
	"context"
	"testing"
)

func TestSetBufferPrefix(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if _, err := c.NewWindow(ctx, WindowSpec{Session: "atl_assistant", Name: "assistant", Argv: []string{"sleep", "300"}}); err != nil {
		t.Fatal(err)
	}
	text := `-n Let's talk; about $HOME "task" a-b.`
	if err := c.SetBuffer(ctx, text); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run(ctx, "show-buffer"); err != nil || out != text {
		t.Fatalf("show-buffer = %q, %v", out, err)
	}
	if k := c.PrefixKey(ctx); k != "Ctrl-b" {
		t.Errorf("prefix = %q", k)
	}
	if _, err := c.run(ctx, "set-option", "-g", "prefix", "C-a"); err != nil {
		t.Fatal(err)
	}
	if k := c.PrefixKey(ctx); k != "Ctrl-a" {
		t.Errorf("prefix = %q", k)
	}
}

func TestNewWindowUntagged(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	id, err := c.NewWindow(ctx, WindowSpec{Session: "atl_assistant", Name: "assistant", Argv: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := c.NewWindow(ctx, WindowSpec{Session: "atl-x", Argv: []string{"sleep", "300"}, Tag: "5"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.run(ctx, "display-message", "-p", "-t", id, "#{"+TagOption+"}")
	if err != nil || out != "" {
		t.Fatalf("untagged window has %s=%q", TagOption, out)
	}
	tags, err := c.TaggedWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags["5"] != tagged {
		t.Fatalf("TaggedWindows = %v", tags)
	}
	for _, v := range tags {
		if v == id {
			t.Fatal("untagged window listed")
		}
	}
	if !c.HasSession(ctx, "atl_assistant") {
		t.Fatal("assistant session gone")
	}
}

func TestWindowAlive(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if ok, err := c.WindowAlive(ctx, "atl_assistant", "assistant"); ok || err != nil {
		t.Fatalf("no server: %v %v", ok, err)
	}
	if _, err := c.run(ctx, "set-option", "-g", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	other, err := c.NewWindow(ctx, WindowSpec{Session: "atl_assistant", Name: "other", Argv: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	// Session alive, but no assistant window (nor a prefix match).
	for _, w := range []string{"assistant", "oth"} {
		if ok, err := c.WindowAlive(ctx, "atl_assistant", w); ok || err != nil {
			t.Fatalf("missing window %s: %v %v", w, ok, err)
		}
	}
	if ok, err := c.WindowAlive(ctx, "atl_assistant", "other"); !ok || err != nil {
		t.Fatalf("other window: %v %v", ok, err)
	}
	id, err := c.NewWindow(ctx, WindowSpec{Session: "atl_assistant", Name: "assistant", Argv: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c.WindowAlive(ctx, "atl_assistant", "assistant"); !ok || err != nil {
		t.Fatalf("live window: %v %v", ok, err)
	}
	// Exact session match only.
	if ok, err := c.WindowAlive(ctx, "atl_assist", "assistant"); ok || err != nil {
		t.Fatalf("session prefix: %v %v", ok, err)
	}
	if _, err := c.run(ctx, "respawn-pane", "-k", "-t", id, "true"); err != nil {
		t.Fatal(err)
	}
	waitPanes(t, c, id, "1")
	if ok, err := c.WindowAlive(ctx, "atl_assistant", "assistant"); ok || err != nil {
		t.Fatalf("dead window: %v %v", ok, err)
	}
	// Another live window does not make the dead one alive.
	if ok, _ := c.WindowAlive(ctx, "atl_assistant", "other"); !ok {
		t.Fatalf("other window %s died", other)
	}
}
