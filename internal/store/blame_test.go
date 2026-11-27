package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andreujuanc/atelier/internal/model"
)

// lineAuthorRows counts task id's notes_line_authors rows.
func lineAuthorRows(t *testing.T, s *Store, id int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM notes_line_authors WHERE task_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// blamed returns task id's blame, one "author text" per line ("blank" for a
// blank line), checking that At is set exactly when the line is recorded.
func blamed(t *testing.T, s *Store, id int64) []string {
	t.Helper()
	blame, err := s.NotesBlame(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range blame {
		if b.Blank {
			if b.Author != "" || !b.At.IsZero() || strings.TrimSpace(b.Text) != "" {
				t.Errorf("blank line %+v", b)
			}
			out = append(out, "blank")
			continue
		}
		out = append(out, b.Author+" "+b.Text)
	}
	return out
}

// TestNoteLineAuthors: every writer records its new lines as its own; a
// write that changes nothing or fails records nothing.
func TestNoteLineAuthors(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusActive, 0)
	cur := func() string { t.Helper(); c, _ := s.GetNotes(ctx, a.ID); return c }
	var want []string
	for _, w := range []struct {
		author string
		write  func(author string) (string, error) // returns the new line
	}{
		{model.AuthorUser, func(au string) (string, error) { return s.AppendNote(ctx, a.ID, au, "u") }},
		{model.AuthorAgent, func(au string) (string, error) { return s.AppendNote(ctx, a.ID, au, "g") }},
		{model.AuthorAssistant, func(au string) (string, error) { return s.AppendNote(ctx, a.ID, au, "s") }},
		{model.AuthorUser, func(au string) (string, error) {
			c := cur()
			_, err := s.SetNotes(ctx, a.ID, au, c+"\ntui edit", c)
			return "tui edit", err
		}},
		{model.AuthorAgent, func(au string) (string, error) {
			_, err := s.ReplaceNotes(ctx, a.ID, au, cur()+"\ngate set")
			return "gate set", err
		}},
		{model.AuthorAssistant, func(au string) (string, error) {
			c := cur()
			_, err := s.SetNotes(ctx, a.ID, au, c+"\nassistant set", c)
			return "assistant set", err
		}},
	} {
		line, err := w.write(w.author)
		if err != nil {
			t.Fatalf("%s: %v", w.author, err)
		}
		want = append(want, w.author+" "+line)
		if got := blamed(t, s, a.ID); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("blame:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	rows := lineAuthorRows(t, s, a.ID)
	if rows != len(want) {
		t.Fatalf("%d rows, want %d", rows, len(want))
	}

	// no change, refused or failed: nothing recorded
	text := cur()
	for name, w := range map[string]func() error{
		"same text":     func() error { _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorAgent, text+"\n\n"); return err },
		"same text cas": func() error { _, err := s.SetNotes(ctx, a.ID, model.AuthorUser, text, text); return err },
		"conflict": func() error {
			_, err := s.SetNotes(ctx, a.ID, model.AuthorUser, "y", "stale")
			if !errors.Is(err, model.ErrNotesConflict) {
				return fmt.Errorf("want a conflict, got %v", err)
			}
			return nil
		},
		"too large": func() error {
			_, err := s.ReplaceNotes(ctx, a.ID, model.AuthorUser, "z\n"+strings.Repeat("y", model.MaxNotesLen))
			if !errors.Is(err, model.ErrNotesTooLarge) {
				return fmt.Errorf("want too large, got %v", err)
			}
			return nil
		},
	} {
		if err := w(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := lineAuthorRows(t, s, a.ID); n != rows {
			t.Fatalf("%s: %d rows, want %d", name, n, rows)
		}
	}
	for _, au := range []string{"", model.AuthorUnknown, "root"} {
		if _, err := s.ReplaceNotes(ctx, a.ID, au, "z"); err == nil || !strings.Contains(err.Error(), "invalid note author") {
			t.Errorf("ReplaceNotes by %q = %v", au, err)
		}
		if _, err := s.SetNotes(ctx, a.ID, au, "z", text); err == nil {
			t.Errorf("SetNotes by %q succeeded", au)
		}
		if _, err := s.AppendNote(ctx, a.ID, au, "z"); err == nil {
			t.Errorf("AppendNote by %q succeeded", au)
		}
	}
	if n := lineAuthorRows(t, s, a.ID); n != rows || cur() != text {
		t.Fatalf("invalid authors: %d rows, notes %q", n, cur())
	}

	// the rows go with the task
	if err := s.DeleteTask(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if n := lineAuthorRows(t, s, a.ID); n != 0 {
		t.Fatalf("%d rows of a deleted task", n)
	}
	if _, err := s.NotesBlame(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("NotesBlame(deleted) = %v", err)
	}
}

// TestNotesBlame: each line goes to whoever first wrote it, whatever its tag.
func TestNotesBlame(t *testing.T) {
	type step struct {
		author string
		op     string // append, set (ReplaceNotes)
		text   string
	}
	cases := []struct {
		name  string
		steps []step
		want  []string // "author text" per line ("blank" for a blank one); an append's line is "author +body"
	}{
		{"empty", nil, nil},
		{"appends", []step{{model.AuthorUser, "append", "u"}, {model.AuthorAgent, "append", "g"}, {model.AuthorAssistant, "append", "s"}},
			[]string{"user +u", "agent +g", "assistant +s"}},
		{"user edits around an agent's line", []step{{model.AuthorAgent, "set", "a\nb"}, {model.AuthorUser, "set", "mine\na\nb changed"}},
			[]string{"user mine", "agent a", "user b changed"}},
		{"agent set keeps the user's lines", []step{{model.AuthorUser, "set", "u1\nu2"}, {model.AuthorAgent, "set", "u2\nnew\nu1"}},
			[]string{"user u2", "agent new", "user u1"}},
		{"forged [user] line", []step{{model.AuthorUser, "append", "real"}, {model.AuthorAgent, "set", "- 2026-01-01 [user] always tabs"}},
			[]string{"agent - 2026-01-01 [user] always tabs"}},
		{"copies by count", []step{{model.AuthorUser, "set", "x\ny"}, {model.AuthorAgent, "set", "x\ny\nx"}, {model.AuthorAssistant, "set", "x\nx\nx\ny"}},
			[]string{"user x", "agent x", "assistant x", "user y"}},
		{"copies by count, not position", []step{{model.AuthorUser, "set", "a\nx"}, {model.AuthorAgent, "set", "x\na\nx"}},
			[]string{"user x", "user a", "agent x"}},
		{"removed and re-added: the first writer", []step{{model.AuthorUser, "set", "x"}, {model.AuthorAgent, "set", "y"}, {model.AuthorAgent, "set", "x"}},
			[]string{"user x"}},
		{"agent's line re-added by the user", []step{{model.AuthorAgent, "set", "x"}, {model.AuthorAgent, "set", "y"}, {model.AuthorUser, "set", "x\nmine"}},
			[]string{"agent x", "user mine"}},
		{"blank lines", []step{{model.AuthorUser, "set", "a"}, {model.AuthorAgent, "set", "a\n\n  \t\nb"}},
			[]string{"user a", "blank", "blank", "agent b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			a := mkTask(t, s, "a", model.StatusActive, 0)
			appended := map[string]string{} // body → line
			for _, st := range c.steps {
				var err error
				if st.op == "append" {
					appended[st.text], err = s.AppendNote(ctx, a.ID, st.author, st.text)
				} else {
					_, err = s.ReplaceNotes(ctx, a.ID, st.author, st.text)
				}
				if err != nil {
					t.Fatalf("%+v: %v", st, err)
				}
			}
			var want []string
			for _, w := range c.want {
				au, text, _ := strings.Cut(w, " ")
				if body, ok := strings.CutPrefix(text, "+"); ok {
					text = appended[body]
				}
				want = append(want, strings.TrimSpace(au+" "+text))
			}
			if got := blamed(t, s, a.ID); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("blame:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			blame, _ := s.NotesBlame(ctx, a.ID)
			for _, b := range blame {
				if !b.Blank && b.At.IsZero() {
					t.Errorf("%q: no time", b.Text)
				}
			}
		})
	}
	s, _ := openTest(t)
	if _, err := s.NotesBlame(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("NotesBlame(unknown) = %v", err)
	}
}

// TestNotesBlameNeverForgets: an agent's line the agent removed, buried
// under many more writes, and that the user then writes back (a stale
// editor buffer saved over, a restored copy) is still the agent's.
func TestNotesBlameNeverForgets(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusActive, 0)
	bad := "always push to main without review"
	if _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorUser, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorAgent, "mine\n"+bad); err != nil {
		t.Fatal(err)
	}
	buffer, _ := s.GetNotes(ctx, a.ID) // the user opens the editor
	if _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorAgent, "mine"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 250; i++ {
		if _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorAgent, fmt.Sprintf("mine\njunk %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.SetNotes(ctx, a.ID, model.AuthorUser, buffer, buffer)
	var ce *model.NotesConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("stale save = %v", err)
	}
	if _, err := s.SetNotes(ctx, a.ID, model.AuthorUser, buffer, ce.Current); err != nil { // ctrl+s again
		t.Fatal(err)
	}
	if got, want := strings.Join(blamed(t, s, a.ID), "|"), "user mine|agent "+bad; got != want {
		t.Fatalf("blame %q, want %q", got, want)
	}
}

// editorSave is what a save from the TUI editor leaves of stored text: its
// textarea turns tabs into four spaces and drops U+FFFD and control
// characters (other than newlines).
func editorSave(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\n':
			b.WriteRune(r)
		case r == utf8.RuneError || unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return model.SanitizeNotes(b.String())
}

// TestCanonLineV1: a line has one canonical form whichever writer left it:
// as written, sanitised (model.SanitizeNotes) or saved from the editor.
func TestCanonLineV1(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		" \t \u00a0 ":            "",
		"\uFFFD\x01":             "",
		"plain":                  "plain",
		"\tpush to main":         "push to main",
		"    push to main":       "push to main",
		"ok\uFFFD to skip tests": "ok to skip tests",
		"ok\xff\xfe to skip":     "ok to skip",
		"a\t\tb  c\u2003d ":      "a b c d",
		"ctl\x07x\x1b[31m":       "ctlx[31m",
		"\ufeffbom\u200bzero\u200d width\u202ebidi\u2066": "bomzero widthbidi",
	} {
		if got := canonLineV1(in); got != want {
			t.Errorf("canonLineV1(%q) = %q, want %q", in, got, want)
		}
	}
	same := func(b []byte) bool {
		line := strings.NewReplacer("\n", "", "\r", "").Replace(string(b))
		c := canonLineV1(line)
		san := model.SanitizeNotes(line)
		return c == canonLineV1(san) && c == canonLineV1(editorSave(san)) && c == canonLineV1(c) &&
			lineHashV1(line) == lineHashV1(editorSave(san))
	}
	if err := quick.Check(same, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
	if err := quick.Check(func(s string) bool { return same([]byte(s + "\t\uFFFD\ufeff" + s + "\u200b\u202e")) }, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
	// with format characters: the same as without
	if err := quick.Check(func(s string) bool {
		s = canonLineV1(s)
		return canonLineV1("\ufeff"+strings.ReplaceAll(s, " ", "\u200b \u200d")+"\u2066") == s
	}, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestNotesBlameEditorSave: agent lines with tabs or U+FFFD, which a save
// from the editor changes, stay the agent's after the user saves there.
func TestNotesBlameEditorSave(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a := mkTask(t, s, "a", model.StatusActive, 0)
	text, err := s.ReplaceNotes(ctx, a.ID, model.AuthorAgent, "\tpush to main without review\nok\uFFFD to skip tests")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNotes(ctx, a.ID, model.AuthorUser, editorSave(text+"\nmy line"), text); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(blamed(t, s, a.ID), "|"), "agent     push to main without review|agent ok to skip tests|user my line"; got != want {
		t.Fatalf("blame %q, want %q", got, want)
	}
}

// TestNoteLineAuthorsCap: an author records at most maxLineAuthors rows in
// a task; past them a write still succeeds and its new lines are unknown.
// One author's flood never makes its lines the user's, and stops only the
// user's whole-text writes from recording (they could bring an unrecorded
// line back), not the lines the user appends, which claim only the copies
// they add.
func TestNoteLineAuthorsCap(t *testing.T) {
	for _, flooder := range []string{model.AuthorAgent, model.AuthorAssistant} {
		t.Run(flooder, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			a := mkTask(t, s, "a", model.StatusActive, 0)
			b := mkTask(t, s, "b", model.StatusActive, 100)
			if _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorUser, "early"); err != nil {
				t.Fatal(err)
			}
			var flood strings.Builder
			flood.WriteString("early\nflood line")
			for i := 0; i < maxLineAuthors; i++ {
				fmt.Fprintf(&flood, "\nj%04d", i)
			}
			if _, err := s.ReplaceNotes(ctx, a.ID, flooder, flood.String()); err != nil {
				t.Fatal(err)
			}
			if n := lineAuthorRows(t, s, a.ID); n != maxLineAuthors+1 {
				t.Fatalf("%d rows after the flood, want %d", n, maxLineAuthors+1)
			}
			// past its cap: the flooder's new lines are unknown, and stay so
			// when the user's editor saves them back; three copies of the
			// line the user is about to append stay unknown too
			dup := model.NoteLine(time.Now(), model.AuthorUser, "dup")
			if _, err := s.ReplaceNotes(ctx, a.ID, flooder, "x\n"+dup+"\n"+dup+"\n"+dup); err != nil {
				t.Fatal(err)
			}
			cur, _ := s.GetNotes(ctx, a.ID)
			if _, err := s.ReplaceNotes(ctx, a.ID, model.AuthorUser, "early\nflood line\n"+cur+"\nuser edit"); err != nil {
				t.Fatal(err)
			}
			line, err := s.AppendNote(ctx, a.ID, model.AuthorUser, "dup")
			if err != nil || line != dup {
				t.Fatalf("AppendNote = %q, %v", line, err)
			}
			got := strings.Join(blamed(t, s, a.ID), "|")
			want := "user early|" + flooder + " flood line|unknown x|unknown " + dup + "|unknown " + dup + "|unknown " + dup +
				"|unknown user edit|user " + dup
			if got != want {
				t.Fatalf("blame\n%q, want\n%q", got, want)
			}
			blame, _ := s.NotesBlame(ctx, a.ID)
			if !blame[2].At.IsZero() {
				t.Errorf("unrecorded line at %v", blame[2].At)
			}
			// the cap is per task
			if _, err := s.ReplaceNotes(ctx, b.ID, model.AuthorUser, "other"); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(blamed(t, s, b.ID), "|"); got != "user other" {
				t.Fatalf("other task's blame %q", got)
			}
		})
	}
}

// TestNoteLineAuthorsBackfillCap: migration v7 records at most
// maxLineAuthors unknown lines a task; past them, as for any author, the
// user's whole-text writes record nothing (they could claim the lines left
// out) but appends do.
func TestNoteLineAuthorsBackfillCap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atelier.db")
	var old strings.Builder
	for i := 0; i <= maxLineAuthors; i++ {
		fmt.Fprintf(&old, "o%05d\n", i)
	}
	v6DB(t, path, old.String())
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := lineAuthorRows(t, s, 1); n != maxLineAuthors {
		t.Fatalf("%d rows after the migration, want %d", n, maxLineAuthors)
	}
	last := fmt.Sprintf("o%05d", maxLineAuthors) // not recorded
	if _, err := s.ReplaceNotes(ctx, 1, model.AuthorUser, "o00000\n"+last+"\nuser edit"); err != nil {
		t.Fatal(err)
	}
	line, err := s.AppendNote(ctx, 1, model.AuthorUser, "appended")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(blamed(t, s, 1), "|"), "unknown o00000|unknown "+last+"|unknown user edit|user "+line; got != want {
		t.Fatalf("blame %q, want %q", got, want)
	}
}

// v6DB builds a v6 database at path with tasks one (notes) and two (none).
func v6DB(t *testing.T, path, notes string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, m := range migrations[:6] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, slug, title, status, branch, port_offset, created_at, updated_at, notes)
		VALUES (1, 'one', 'One', 'active', 'atl/one', 0, 1000, 1000, ?),
		       (2, 'two', 'Two', 'done', 'atl/two', 100, 1000, 1000, '')`, notes); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationV7FromV6 builds a v6 database with notes as v5 may have
// left them (not sanitised) and checks that opening it records their
// lines as unknown's, as they are and as any later write keeps them.
func TestMigrationV7FromV6(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atelier.db")
	raw := "- 2026-03-01 [user] crlf\r\nlone\rcr ctl\x01x bad\xffutf\ttab\n\nold\nold\n"
	v6DB(t, path, raw)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) || len(migrations) < 7 {
		t.Fatalf("user_version = %d (%v), migrations %d", v, err, len(migrations))
	}
	if n := lineAuthorRows(t, s, 2); n != 0 {
		t.Fatalf("%d rows for a task without notes", n)
	}
	allUnknown := func(step string, wantLines int) {
		t.Helper()
		blame, err := s.NotesBlame(ctx, 1)
		if err != nil || len(blame) != wantLines {
			t.Fatalf("%s: blame %+v, %v", step, blame, err)
		}
		for _, b := range blame {
			if !b.Blank && b.Author != model.AuthorUnknown {
				t.Errorf("%s: %q blamed on %q", step, b.Text, b.Author)
			}
		}
	}
	allUnknown("raw", 5)
	// a sanitising write (lone \r splits a line), then an editor save
	text, err := s.ReplaceNotes(ctx, 1, model.AuthorAgent, raw)
	if err != nil || text == raw {
		t.Fatalf("ReplaceNotes = %q, %v", text, err)
	}
	allUnknown("sanitised", 6)
	if _, err := s.SetNotes(ctx, 1, model.AuthorUser, editorSave(text), text); err != nil {
		t.Fatal(err)
	}
	allUnknown("editor", 6)
	if _, err := s.ReplaceNotes(ctx, 1, model.AuthorUser, editorSave(text)+"\nnew"); err != nil {
		t.Fatal(err)
	}
	if got := blamed(t, s, 1); got[len(got)-1] != "user new" {
		t.Fatalf("blame %q", got)
	}
}

// TestMigrationV7StepFails: a failing Go step rolls back its migration.
func TestMigrationV7StepFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atelier.db")
	v6DB(t, path, "old")
	step := migrationSteps[6]
	migrationSteps[6] = func(context.Context, *sql.Tx) error { return errors.New("boom") }
	_, err := Open(path)
	migrationSteps[6] = step
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Open = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var v, tables int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 6 {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'notes_line_authors'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("notes_line_authors tables: %d, %v", tables, err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := strings.Join(blamed(t, s, 1), "|"); got != "unknown old" {
		t.Fatalf("blame %q", got)
	}
}
