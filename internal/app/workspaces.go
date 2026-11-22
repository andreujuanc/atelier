package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/andreujuanc/atelier/internal/model"
	"github.com/andreujuanc/atelier/internal/store"
)

// WorkspaceCounts returns the open tasks (backlog, active, review) and the
// waiting sessions in the database of the workspace in dir
// (<dir>/data/atelier.db), read read-only; 0, 0 when it has no database.
func WorkspaceCounts(ctx context.Context, dir string) (open, waiting int, err error) {
	db := filepath.Join(dir, "data", "atelier.db")
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	st, err := store.OpenReadOnly(db, 2*time.Second)
	if err != nil {
		return 0, 0, err
	}
	defer st.Close()
	tasks, err := st.ListTasks(ctx, model.StatusBacklog, model.StatusActive, model.StatusReview)
	if err != nil {
		return 0, 0, err
	}
	sessions, err := st.ListSessionsByState(ctx, model.StateWaiting)
	if err != nil {
		return 0, 0, err
	}
	return len(tasks), len(sessions), nil
}
