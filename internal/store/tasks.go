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

const taskCols = `id, slug, title, description, status, project, branch, port_offset, base, created_at, updated_at`

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
		&t.Branch, &t.PortOffset, &t.Base, &created, &updated); err != nil {
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
	if err := validateTask(t); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertTask(ctx, tx, t); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateTaskAutoPort inserts t and its repos like CreateTask, but first
// allocates, inside the same BEGIN IMMEDIATE transaction, the lowest free
// port offset (as NextPortOffset) and a unique slug derived from t.Slug (as
// UniqueSlug), so concurrent callers never get the same slug or offset. It
// sets t.ID, t.Slug, t.Branch ("atl/"+slug), t.PortOffset and timestamps.
func (s *Store) CreateTaskAutoPort(ctx context.Context, t *model.Task) error {
	if err := validateTask(t); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock): takes the write lock now
	if err != nil {
		return err
	}
	defer tx.Rollback()
	slug, err := uniqueSlug(ctx, tx, t.Slug)
	if err != nil {
		return err
	}
	port, err := nextPortOffset(ctx, tx)
	if err != nil {
		return err
	}
	cp := *t
	cp.Slug, cp.Branch, cp.PortOffset = slug, "atl/"+slug, port
	if err := insertTask(ctx, tx, &cp); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*t = cp
	return nil
}

// DeleteTask deletes a task; its repos and sessions go with it (ON DELETE
// CASCADE).
func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return checkAffected(res, "task", id)
}

func validateTask(t *model.Task) error {
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
	return nil
}

// insertTask inserts t and its repos within tx and sets t.ID and timestamps.
func insertTask(ctx context.Context, tx *sql.Tx, t *model.Task) error {
	created, updated := normTime(t.CreatedAt), normTime(t.UpdatedAt)
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks
		(slug, title, description, status, project, branch, port_offset, base, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Slug, t.Title, t.Description, string(t.Status), t.Project, t.Branch, t.PortOffset, t.Base,
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
	t.ID, t.CreatedAt, t.UpdatedAt = id, created, updated
	return nil
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// getTask loads one task matching where/args, with repos.
func (s *Store) getTask(ctx context.Context, desc, where string, args ...any) (*model.Task, error) {
	t, err := s.getTaskBrief(ctx, desc, where, args...)
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

// GetTaskBrief returns the task with the given id without its repos (Repos
// is nil): one query, for hot paths such as `atl statusline`.
func (s *Store) GetTaskBrief(ctx context.Context, id int64) (*model.Task, error) {
	return s.getTaskBrief(ctx, strconv.FormatInt(id, 10), "id = ?", id)
}

// GetTaskBriefBySlug is GetTaskBrief by slug.
func (s *Store) GetTaskBriefBySlug(ctx context.Context, slug string) (*model.Task, error) {
	return s.getTaskBrief(ctx, strconv.Quote(slug), "slug = ?", slug)
}

func (s *Store) getTaskBrief(ctx context.Context, desc, where string, args ...any) (*model.Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("task %s: %w", desc, ErrNotFound)
	}
	return t, err
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

// UpdateTaskTitle sets a task's title and bumps updated_at; nothing else
// changes (slug, branch and dirs keep the original name). The title must
// already be in its stored form: model.NormalizeTitle must return it
// unchanged.
func (s *Store) UpdateTaskTitle(ctx context.Context, id int64, title string) error {
	if t, err := model.NormalizeTitle(title); err != nil {
		return fmt.Errorf("store: %w", err)
	} else if t != title {
		return fmt.Errorf("store: title %q is not normalized", title)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET title = ?, updated_at = ? WHERE id = ?`,
		title, toMS(now()), id)
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
	return uniqueSlug(ctx, s.db, base)
}

func uniqueSlug(ctx context.Context, q querier, base string) (string, error) {
	taken := func(slug string) (bool, error) {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE slug = ?`, slug).Scan(&one)
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
	return nextPortOffset(ctx, s.db)
}

func nextPortOffset(ctx context.Context, q querier) (int, error) {
	var open []any
	for _, st := range model.AllStatuses {
		if st.Open() {
			open = append(open, string(st))
		}
	}
	rows, err := q.QueryContext(ctx,
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

// ErrDuplicate is returned (wrapped) by AddTaskRepo when the task already
// has a repo with that name.
var ErrDuplicate = errors.New("already exists")

// ErrTaskClosed is returned (wrapped) by AddTaskRepo when the task is no
// longer open (done or cancelled).
var ErrTaskClosed = errors.New("task is closed")

// AddTaskRepo appends repo r to task taskID (position = current max + 1)
// and bumps the task's updated_at, in one transaction. It returns
// ErrNotFound for an unknown task, ErrTaskClosed if the task is not open
// (backlog, active or review) and ErrDuplicate if the task already has a
// repo named r.Name.
func (s *Store) AddTaskRepo(ctx context.Context, taskID int64, r model.TaskRepo) error {
	if r.Name == "" {
		return errors.New("store: task repo name is empty")
	}
	if !r.Mode.Valid() {
		return fmt.Errorf("store: invalid mode %q for repo %q", r.Mode, r.Name)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM task_repos WHERE task_id = ? AND name = ?`, taskID, r.Name).Scan(&one)
	if err == nil {
		return fmt.Errorf("store: repo %q of task %d: %w", r.Name, taskID, ErrDuplicate)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := touchOpenTask(ctx, tx, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_repos
		(task_id, name, mode, base_branch, base_commit, position)
		VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM task_repos WHERE task_id = ?))`,
		taskID, r.Name, string(r.Mode), r.BaseBranch, r.BaseCommit, taskID); err != nil {
		return fmt.Errorf("store: insert repo %q for task %d: %w", r.Name, taskID, err)
	}
	return tx.Commit()
}

// UpgradeTaskRepo turns ro repo r.Name of task taskID into rw with r's base
// branch and commit, keeping its position. Like AddTaskRepo it bumps the
// task's updated_at and requires an open task (ErrTaskClosed); a repo the
// task does not have as ro is ErrNotFound.
func (s *Store) UpgradeTaskRepo(ctx context.Context, taskID int64, r model.TaskRepo) error {
	if r.Mode != model.ModeRW {
		return fmt.Errorf("store: upgrade repo %q to mode %q: only rw", r.Name, r.Mode)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := touchOpenTask(ctx, tx, taskID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE task_repos SET mode = 'rw', base_branch = ?, base_commit = ?
		WHERE task_id = ? AND name = ? AND mode = 'ro'`, r.BaseBranch, r.BaseCommit, taskID, r.Name)
	if err != nil {
		return fmt.Errorf("store: upgrade repo %q for task %d: %w", r.Name, taskID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("store: ro repo %q of task %d: %w", r.Name, taskID, ErrNotFound)
	}
	return tx.Commit()
}

// touchOpenTask bumps task taskID's updated_at in tx, or returns
// ErrTaskClosed (done/cancelled) or ErrNotFound.
func touchOpenTask(ctx context.Context, tx *sql.Tx, taskID int64) error {
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ?
		WHERE id = ? AND status IN ('backlog', 'active', 'review')`, toMS(now()), taskID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var st string
		err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&st)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("task %d: %w", taskID, ErrNotFound)
		case err != nil:
			return err
		}
		return fmt.Errorf("store: task %d is %s: %w", taskID, st, ErrTaskClosed)
	}
	return nil
}
