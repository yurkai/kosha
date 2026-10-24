package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"math/big"
	"strings"

	_ "modernc.org/sqlite"
)

const roomAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const roomCodeLength = 6

type roomRegistryStore struct {
	db     *sql.DB
	random io.Reader
}

func openRoomRegistry(path string, random io.Reader) (*roomRegistryStore, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS room_codes (id TEXT PRIMARY KEY)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &roomRegistryStore{db: db, random: random}, nil
}

func (r *roomRegistryStore) Close() error { return r.db.Close() }

func (r *roomRegistryStore) Create(ctx context.Context) (string, error) {
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
			INSERT INTO room_codes (id)
			SELECT ? WHERE NOT EXISTS (SELECT 1 FROM room_codes WHERE id = ? COLLATE NOCASE)
			AND NOT EXISTS (SELECT 1 FROM updates WHERE room = ? COLLATE NOCASE)
			ON CONFLICT DO NOTHING`, string(code), string(code), string(code))
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
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO room_codes (id) VALUES (?)`, room)
	return err
}

func (r *roomRegistryStore) Lookup(ctx context.Context, room string) (string, error) {
	lookup := func(name string) (bool, error) {
		var found bool
		err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_codes WHERE id = ?) OR EXISTS (SELECT 1 FROM updates WHERE room = ?)`, name, name).Scan(&found)
		return found, err
	}
	if found, err := lookup(room); err != nil {
		return "", err
	} else if found {
		return room, nil
	}
	upper := strings.ToUpper(room)
	if len(room) == roomCodeLength && upper != room && strings.IndexFunc(upper, func(ch rune) bool { return !strings.ContainsRune(roomAlphabet, ch) }) == -1 {
		if found, err := lookup(upper); err != nil {
			return "", err
		} else if found {
			return upper, nil
		}
	}
	return "", nil
}
