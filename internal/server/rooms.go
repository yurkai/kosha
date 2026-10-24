package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"math/big"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const roomAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const roomCodeLength = 6

type roomRegistryStore struct {
	db       *sql.DB
	random   io.Reader
	ttl      time.Duration
	now      func() time.Time
	mu       sync.Mutex
	active   map[string]*roomActivity
	stopping bool
	sessions sync.WaitGroup
}

func openRoomRegistry(path string, random io.Reader, ttl time.Duration, now func() time.Time) (*roomRegistryStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	r := &roomRegistryStore{db: db, random: random, ttl: ttl, now: now, active: make(map[string]*roomActivity)}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

func (r *roomRegistryStore) Close() error { return r.db.Close() }

func (r *roomRegistryStore) Create(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for range 32 {
		code := make([]byte, roomCodeLength)
		for i := range code {
			n, err := rand.Int(r.random, big.NewInt(int64(len(roomAlphabet))))
			if err != nil {
				return "", err
			}
			code[i] = roomAlphabet[n.Int64()]
		}
		// The check and reservation are one SQLite statement. Include documents
		// from before the registry existed, and never reuse a case variant.
		result, err := r.db.ExecContext(ctx, `
			INSERT INTO room_codes (id, last_used_at)
			SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM room_codes WHERE id = ? COLLATE NOCASE)
			AND NOT EXISTS (SELECT 1 FROM updates WHERE room = ? COLLATE NOCASE)
			ON CONFLICT DO NOTHING`, string(code), r.now().UnixNano(), string(code), string(code))
		if err != nil {
			return "", err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return "", err
		}
		if rows == 1 {
			return string(code), nil
		}
	}
	return "", errors.New("could not allocate an unused room code")
}

func (r *roomRegistryStore) Remember(ctx context.Context, room string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rememberLocked(ctx, room)
}

func (r *roomRegistryStore) rememberLocked(ctx context.Context, room string) error {
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO room_codes (id, last_used_at) VALUES (?, ?)`, room, r.now().UnixNano())
	return err
}

func (r *roomRegistryStore) Lookup(ctx context.Context, room string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookupLocked(ctx, room)
}

func (r *roomRegistryStore) lookupLocked(ctx context.Context, room string) (string, error) {
	names := []string{room}
	upper := strings.ToUpper(room)
	if len(room) == roomCodeLength && upper != room && strings.IndexFunc(upper, func(ch rune) bool { return !strings.ContainsRune(roomAlphabet, ch) }) == -1 {
		names = append(names, upper)
	}
	for _, name := range names {
		var lastUsed int64
		var expired sql.NullInt64
		var active bool
		err := r.db.QueryRowContext(ctx, `SELECT last_used_at, expired_at, is_active FROM room_codes WHERE id = ?`, name).Scan(&lastUsed, &expired, &active)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		if expired.Valid {
			return name, errRoomExpired
		}
		if !active && lastUsed <= r.now().Add(-r.ttl).UnixNano() {
			if err := r.expireLocked(ctx, name); err != nil {
				return "", err
			}
			return name, errRoomExpired
		}
		return name, nil
	}
	// Never resurrect a case variant of a retired legacy identifier either.
	var retired bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_codes WHERE id = ? COLLATE NOCASE AND expired_at IS NOT NULL)`, room).Scan(&retired); err != nil {
		return "", err
	}
	if retired {
		return "", errRoomExpired
	}
	return "", nil
}
