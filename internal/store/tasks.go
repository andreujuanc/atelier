package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/andreujuanc/atelier/internal/model"
)

const taskCols = `id, slug, title, description, status, project, branch, port_offset, created_at, updated_at`

// statusOrder sorts tasks in model.AllStatuses order.
var statusOrder = func() string {
	var b strings.Builder
	b.WriteString("CASE status")
	for i, st := range model.AllStatuses {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", st, i)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(model.AllStatuses))
	return b.String()
}()

func scanTask(sc scanner) (*model.Task, error) {
	var t model.Task
	var status string
	var created, updated int64
	if err := sc.Scan(&t.ID, &t.Slug, &t.Title, &t.Description, &status, &t.Project,
		&t.Branch, &t.PortOffset, &created, &updated); err != nil {
		return nil, err
	}
	t.Status = model.Status(status)
	t.CreatedAt = msTime(created)
	t.UpdatedAt = msTime(updated)
	return &t, nil
}

// CreateTask inserts t and its repos in one transaction, setting t.ID and
// filling zero CreatedAt/UpdatedAt with now.
func (s *Store) CreateTask(ctx context.Context, t *model.Task) error {
	if t.Slug == "" {
		return errors.New("store: task slug is empty")
	}
	if !t.Status.Valid() {
		return fmt.Errorf("store: invalid task status %q", t.Status)
	}
	for _, r := range t.Repos {
		if r.Name == "" {
			return errors.New("store: task repo name is empty")
		}
		if !r.Mode.Valid() {
			return fmt.Errorf("store: invalid mode %q for repo %q", r.Mode, r.Name)
		}
	}
	created, updated := normTime(t.CreatedAt), normTime(t.UpdatedAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks
		(slug, title, description, status, project, branch, port_offset, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Slug, t.Title, t.Description, string(t.Status), t.Project, t.Branch, t.PortOffset,
		toMS(created), toMS(updated))
	if err != nil {
		return fmt.Errorf("store: insert task %q: %w", t.Slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for i, r := range t.Repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_repos
			(task_id, name, mode, base_branch, base_commit, position) VALUES (?, ?, ?, ?, ?, ?)`,
			id, r.Name, string(r.Mode), r.BaseBranch, r.BaseCommit, i); err != nil {
			return fmt.Errorf("store: insert repo %q for task %q: %w", r.Name, t.Slug, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.ID, t.CreatedAt, t.UpdatedAt = id, created, updated
	return nil
}

// getTask loads one task matching where/args, with repos.
func (s *Store) getTask(ctx context.Context, desc, where string, args ...any) (*model.Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task %s: %w", desc, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadRepos(ctx, []*model.Task{t}, "task_id = ?", t.ID); err != nil {
		return nil, err
	}
	return t, nil
}

// loadRepos fills Repos for tasks, querying task_repos rows matching where.
func (s *Store) loadRepos(ctx context.Context, tasks []*model.Task, where string, args ...any) error {
	if len(tasks) == 0 {
		return nil
	}
	byID := make(map[int64]*model.Task, len(tasks))
	for _, t := range tasks {
		t.Repos = []model.TaskRepo{}
		byID[t.ID] = t
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, name, mode, base_branch, base_commit
		FROM task_repos WHERE `+where+` ORDER BY task_id, position`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r model.TaskRepo
		var mode string
		if err := rows.Scan(&id, &r.Name, &mode, &r.BaseBranch, &r.BaseCommit); err != nil {
			return err
		}
		r.Mode = model.Mode(mode)
		if t, ok := byID[id]; ok {
			t.Repos = append(t.Repos, r)
		}
	}
	return rows.Err()
}

// GetTask returns the task with the given id, including its repos.
func (s *Store) GetTask(ctx context.Context, id int64) (*model.Task, error) {
	return s.getTask(ctx, strconv.FormatInt(id, 10), "id = ?", id)
}

// GetTaskBySlug returns the task with the given slug, including its repos.
func (s *Store) GetTaskBySlug(ctx context.Context, slug string) (*model.Task, error) {
	return s.getTask(ctx, strconv.Quote(slug), "slug = ?", slug)
}

// ResolveTask finds a task by user-supplied reference: exact slug, else
// numeric id (if ref is all digits), else unique slug prefix. Returns
// ErrAmbiguous if several slugs share the prefix, ErrNotFound if none.
func (s *Store) ResolveTask(ctx context.Context, ref string) (*model.Task, error) {
	if ref == "" {
		return nil, fmt.Errorf("task %q: %w", ref, ErrNotFound)
	}
	t, err := s.GetTaskBySlug(ctx, ref)
	if !errors.Is(err, ErrNotFound) {
		return t, err
	}
	if isDigits(ref) {
		if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
			t, err := s.GetTask(ctx, id)
			if !errors.Is(err, ErrNotFound) {
				return t, err
			}
		}
		// No such id (or out of range): fall through to prefix matching.
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, slug FROM tasks WHERE substr(slug, 1, length(?1)) = ?1 ORDER BY slug LIMIT 5`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	var slugs []string
	for rows.Next() {
		var id int64
		var slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("task %q: %w", ref, ErrNotFound)
	case 1:
		return s.GetTask(ctx, ids[0])
	default:
		return nil, fmt.Errorf("task %q matches %s: %w", ref, strings.Join(slugs, ", "), ErrAmbiguous)
	}
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// ListTasks returns tasks with any of the given statuses (all tasks if none
// given), ordered by status (model.AllStatuses order) then updated_at desc,
// with repos loaded.
func (s *Store) ListTasks(ctx context.Context, statuses ...model.Status) ([]model.Task, error) {
	where := "1"
	var args []any
	if len(statuses) > 0 {
		for _, st := range statuses {
			if !st.Valid() {
				return nil, fmt.Errorf("store: invalid task status %q", st)
			}
			args = append(args, string(st))
		}
		where = "status IN (" + placeholders(len(statuses)) + ")"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE `+where+
		` ORDER BY `+statusOrder+`, updated_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ptrs []*model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		ptrs = append(ptrs, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.loadRepos(ctx, ptrs, "task_id IN (SELECT id FROM tasks WHERE "+where+")", args...); err != nil {
		return nil, err
	}
	out := make([]model.Task, len(ptrs))
	for i, t := range ptrs {
		out[i] = *t
	}
	return out, nil
}

// SetTaskStatus sets a task's status and bumps updated_at.
func (s *Store) SetTaskStatus(ctx context.Context, id int64, st model.Status) error {
	if !st.Valid() {
		return fmt.Errorf("store: invalid task status %q", st)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
		string(st), toMS(now()), id)
	if err != nil {
		return err
	}
	return checkAffected(res, "task", id)
}

// TouchTask sets a task's updated_at to now.
func (s *Store) TouchTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, toMS(now()), id)
	if err != nil {
		return err
	}
	return checkAffected(res, "task", id)
}

// maxSlugLen mirrors the ≤40-char slug limit enforced by model.Slugify.
const maxSlugLen = 40

// UniqueSlug returns base if no task uses it, else the first free of
// base-2, base-3, ... Results never exceed 40 chars: base is trimmed (along
// with any trailing "-") to make room for the suffix.
func (s *Store) UniqueSlug(ctx context.Context, base string) (string, error) {
	taken := func(slug string) (bool, error) {
		var one int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE slug = ?`, slug).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	c := trimSlug(base, maxSlugLen)
	for i := 1; ; i++ {
		if i > 1 {
			suffix := fmt.Sprintf("-%d", i)
			c = trimSlug(base, maxSlugLen-len(suffix)) + suffix
		}
		t, err := taken(c)
		if err != nil {
			return "", err
		}
		if !t {
			return c, nil
		}
	}
}

// trimSlug cuts slug to at most n bytes and drops any trailing "-".
func trimSlug(slug string, n int) string {
	if len(slug) > n {
		slug = strings.TrimRight(slug[:n], "-")
	}
	return slug
}

// NextPortOffset returns the lowest multiple of 100 (from 0) not used by an
// open (backlog/active/review) task.
func (s *Store) NextPortOffset(ctx context.Context) (int, error) {
	var open []any
	for _, st := range model.AllStatuses {
		if st.Open() {
			open = append(open, string(st))
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT port_offset FROM tasks WHERE status IN (`+placeholders(len(open))+`)`, open...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var off int
		if err := rows.Scan(&off); err != nil {
			return 0, err
		}
		used[off] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	off := 0
	for used[off] {
		off += 100
	}
	return off, nil
}
