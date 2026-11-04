package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
)

// fileSnap is a file's contents and modification time.
type fileSnap struct {
	data  []byte
	mtime time.Time
}

func snap(t *testing.T, path string) fileSnap {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileSnap{b, fi.ModTime()}
}

func (a fileSnap) same(b fileSnap) bool { return bytes.Equal(a.data, b.data) && a.mtime.Equal(b.mtime) }

// rawVersion reads user_version without going through Open (no migration).
func rawVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// makeAtVersion creates a WAL database at path with the first v migrations
// applied, as an older atl binary would have left it.
func makeAtVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:v] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyCurrent(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	task := mkTask(t, s, "keep", model.StatusActive, 0)
	s.Close() // last connection: WAL checkpointed into the main file
	before := snap(t, path)

	ro, err := OpenReadOnly(path, 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ro.GetTaskBriefBySlug(ctx, "keep"); err != nil || got.ID != task.ID {
		t.Fatalf("read: %v, %v", got, err)
	}
	var qo, bt int
	var jm string
	ro.db.QueryRow("PRAGMA query_only").Scan(&qo)
	ro.db.QueryRow("PRAGMA busy_timeout").Scan(&bt)
	ro.db.QueryRow("PRAGMA journal_mode").Scan(&jm)
	if qo != 1 || bt != 250 || jm != "wal" {
		t.Errorf("query_only=%d busy_timeout=%d journal_mode=%s", qo, bt, jm)
	}
	if err := ro.SetTaskStatus(ctx, task.ID, model.StatusDone); err == nil {
		t.Error("write through a read-only store succeeded")
	}
	ro.Close()
	if !before.same(snap(t, path)) {
		t.Error("database file changed by a read-only open")
	}

	// Open (the hooks' path) is unaffected: it still opens read-write.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.SetTaskStatus(ctx, task.ID, model.StatusDone); err != nil {
		t.Errorf("write after a read-only open: %v", err)
	}
}

func TestOpenReadOnlyNeverMigrates(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name    string
		make    func(path string)
		version int
	}{
		{"older (v4)", func(p string) { makeAtVersion(t, p, 4) }, 4},
		{"no schema (v0)", func(p string) { makeAtVersion(t, p, 0) }, 0},
		{"newer", func(p string) {
			makeAtVersion(t, p, len(migrations))
			db, _ := sql.Open("sqlite", p)
			defer db.Close()
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
				t.Fatal(err)
			}
		}, len(migrations) + 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".db")
			c.make(path)
			before := snap(t, path)
			ro, err := OpenReadOnly(path, 0)
			if !errors.Is(err, ErrSchemaVersion) {
				if ro != nil {
					ro.Close()
				}
				t.Fatalf("err = %v, want ErrSchemaVersion", err)
			}
			if !before.same(snap(t, path)) {
				t.Error("database file changed")
			}
			if v := rawVersion(t, path); v != c.version {
				t.Errorf("user_version = %d, want %d", v, c.version)
			}
		})
	}

	t.Run("zero-byte file", func(t *testing.T) {
		path := filepath.Join(dir, "empty.db")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		before := snap(t, path)
		if _, err := OpenReadOnly(path, 0); !errors.Is(err, ErrSchemaVersion) {
			t.Fatalf("err = %v, want ErrSchemaVersion", err)
		}
		if !before.same(snap(t, path)) {
			t.Error("empty database file changed")
		}
		for _, sfx := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Stat(path + sfx); err == nil {
				t.Errorf("%s created", sfx)
			}
		}
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(dir, "sub", "none.db")
		if ro, err := OpenReadOnly(path, 0); err == nil {
			ro.Close()
			t.Fatal("opened a missing database")
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Errorf("directory created: %v", err)
		}
	})
}

// TestOpenReadOnlyPathEscaping: characters that mean something in a URI
// ('#', '%', spaces) are escaped, so the right file is opened.
func TestOpenReadOnlyPathEscaping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a #b%20c")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "atelier.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mkTask(t, s, "here", model.StatusActive, 0)
	s.Close()
	ro, err := OpenReadOnly(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.GetTaskBriefBySlug(context.Background(), "here"); err != nil {
		t.Error(err)
	}
}
