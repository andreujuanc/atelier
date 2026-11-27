package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LearnDigest is a task's last pre-clean (atl learn, see learn.Check).
type LearnDigest struct {
	TaskID          int64
	InputHash       string // the extract's (learn.Extract.InputHash)
	CacheKey        string // learn.CacheKey of the run that made it
	OccurrencesJSON string // the occurrences kept, a JSON array
	CreatedAt       time.Time
	CostUSD         float64
}

// GetLearnDigest returns task id's digest; ok is false when it has none.
func (s *Store) GetLearnDigest(ctx context.Context, id int64) (d LearnDigest, ok bool, err error) {
	var created int64
	err = s.db.QueryRowContext(ctx, `SELECT task_id, input_hash, cache_key, occurrences_json, created_at, cost_usd
		FROM learn_digests WHERE task_id = ?`, id).Scan(&d.TaskID, &d.InputHash, &d.CacheKey, &d.OccurrencesJSON, &created, &d.CostUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return LearnDigest{}, false, nil
	}
	if err != nil {
		return LearnDigest{}, false, err
	}
	d.CreatedAt = msTime(created)
	return d, true, nil
}

// PutLearnDigest stores d as its task's digest, replacing the one before;
// CreatedAt zero is now.
func (s *Store) PutLearnDigest(ctx context.Context, d LearnDigest) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_digests (task_id, input_hash, cache_key, occurrences_json, created_at, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET input_hash = excluded.input_hash, cache_key = excluded.cache_key,
			occurrences_json = excluded.occurrences_json, created_at = excluded.created_at, cost_usd = excluded.cost_usd`,
		d.TaskID, d.InputHash, d.CacheKey, d.OccurrencesJSON, toMS(d.CreatedAt), d.CostUSD)
	return err
}

// DeleteLearnDigest removes task id's digest, if it has one.
func (s *Store) DeleteLearnDigest(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM learn_digests WHERE task_id = ?`, id)
	return err
}
