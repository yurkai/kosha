package server

import (
	"context"
	"errors"
	"time"
)

var errRoomExpired = errors.New("room has expired")
var errServerClosing = errors.New("server is shutting down")

type roomActivity struct{ sessions, loaded int }

func (r *roomRegistryStore) migrate() error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS room_codes (id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	rows, err := tx.Query(`PRAGMA table_info(room_codes)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"last_used_at", "INTEGER"}, {"expired_at", "INTEGER"}, {"is_active", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if !columns[column.name] {
			if _, err := tx.Exec("ALTER TABLE room_codes ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	now := r.now().UnixNano()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO room_codes (id, last_used_at)
		SELECT room, ? FROM (SELECT room FROM updates UNION SELECT room FROM snapshots
		UNION SELECT room FROM snapshot_versions UNION SELECT room FROM checkpoints)`, now); err != nil {
		return err
	}
	// A legacy document has no reliable visit time. A crashed process also
	// cannot report when its active peers left. Give both a full grace period.
	if _, err := tx.Exec(`UPDATE room_codes SET last_used_at = ?, is_active = 0
		WHERE expired_at IS NULL AND (last_used_at IS NULL OR is_active = 1)`, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS room_expiry ON room_codes(last_used_at)
		WHERE expired_at IS NULL AND is_active = 0`); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *roomRegistryStore) Visit(ctx context.Context, room string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, err := r.lookupLocked(ctx, room)
	if err != nil || name == "" {
		return name, err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE room_codes SET last_used_at = ? WHERE id = ?`, r.now().UnixNano(), name)
	return name, err
}

// OpenLink also supports old fragment links, which used to create their own
// identifiers on first use. A tombstone always wins over that legacy behavior.
func (r *roomRegistryStore) OpenLink(ctx context.Context, room string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, err := r.lookupLocked(ctx, room)
	if err != nil {
		return name, err
	}
	if name == "" {
		name = room
		if err := r.rememberLocked(ctx, name); err != nil {
			return "", err
		}
	}
	_, err = r.db.ExecContext(ctx, `UPDATE room_codes SET last_used_at = ? WHERE id = ?`, r.now().UnixNano(), name)
	return name, err
}

// A lease lasts until ygo's ServeHTTP returns, including persistence drain.
// A separate loaded count protects documents retained after a failed flush.
func (r *roomRegistryStore) BeginSession(ctx context.Context, room string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return "", errServerClosing
	}
	name, err := r.lookupLocked(ctx, room)
	if err != nil {
		return name, err
	}
	if name == "" {
		name = room
		if err := r.rememberLocked(ctx, name); err != nil {
			return "", err
		}
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE room_codes SET is_active = 1 WHERE id = ?`, name); err != nil {
		return "", err
	}
	if r.active[name] == nil {
		r.active[name] = &roomActivity{}
	}
	r.active[name].sessions++
	r.sessions.Add(1)
	return name, nil
}

func (r *roomRegistryStore) EndSession(ctx context.Context, room string, participated bool) error {
	defer r.sessions.Done()
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.active[room]
	state.sessions--
	if participated {
		if _, err := r.db.ExecContext(ctx, `UPDATE room_codes SET last_used_at = ? WHERE id = ?`, r.now().UnixNano(), room); err != nil {
			return err
		}
	}
	return r.releaseLocked(ctx, room, state, false)
}

func (r *roomRegistryStore) Loaded(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active[room].loaded++
}

func (r *roomRegistryStore) Unloaded(ctx context.Context, room string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.active[room]
	state.loaded--
	return r.releaseLocked(ctx, room, state, true)
}

func (r *roomRegistryStore) releaseLocked(ctx context.Context, room string, state *roomActivity, drained bool) error {
	if state.sessions != 0 || state.loaded != 0 {
		return nil
	}
	// If the last session already returned while a dirty doc stayed resident,
	// start the full retention period only after persistence finally finishes.
	query := `UPDATE room_codes SET is_active = 0 WHERE id = ?`
	args := []any{room}
	if drained {
		query = `UPDATE room_codes SET is_active = 0, last_used_at = ? WHERE id = ?`
		args = []any{r.now().UnixNano(), room}
	}
	if _, err := r.db.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	delete(r.active, room)
	return nil
}

func (r *roomRegistryStore) StopSessions() {
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
}

func (r *roomRegistryStore) WaitSessions(ctx context.Context) error {
	done := make(chan struct{})
	go func() { r.sessions.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *roomRegistryStore) expireLocked(ctx context.Context, room string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Match the pinned ygo SQLite schema. Data deletion and the permanent
	// tombstone must commit together, preventing resurrection after a crash.
	for _, table := range []string{"updates", "snapshots", "snapshot_versions", "checkpoints"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE room = ?", room); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE room_codes SET expired_at = ? WHERE id = ?`, r.now().UnixNano(), room); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *roomRegistryStore) Sweep(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := r.sweepBatch(ctx)
		total += n
		if err != nil || n < 100 {
			return total, err
		}
	}
}

func (r *roomRegistryStore) sweepBatch(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM room_codes
		WHERE expired_at IS NULL AND is_active = 0 AND last_used_at <= ? LIMIT 100`, r.now().Add(-r.ttl).UnixNano())
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for i, name := range names {
		if err := r.expireLocked(ctx, name); err != nil {
			return i, err
		}
	}
	return len(names), nil
}

func cleanupInterval(ttl time.Duration) time.Duration {
	return min(time.Hour, max(100*time.Millisecond, ttl/2))
}
