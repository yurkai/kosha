package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence/sqlite"
)

type testClock struct{ nanos atomic.Int64 }

func newTestClock() *testClock {
	c := &testClock{}
	c.nanos.Store(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *testClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *testClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

func policyRegistry(t *testing.T, ttl time.Duration, clock *testClock) (*roomRegistryStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rooms.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r, err := openRoomRegistry(path, rand.Reader, ttl, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, path
}

func TestRetentionBoundaryVisitAndReadOnlyLookup(t *testing.T) {
	clock := newTestClock()
	r, _ := policyRegistry(t, 14*24*time.Hour, clock)
	ctx := context.Background()
	room, err := r.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(13 * 24 * time.Hour)
	if got, err := r.Visit(ctx, room); err != nil || got != room {
		t.Fatalf("visit=%q %v", got, err)
	}
	clock.Advance(14*24*time.Hour - time.Nanosecond)
	if got, err := r.Lookup(ctx, room); err != nil || got != room {
		t.Fatalf("before boundary=%q %v", got, err)
	}
	clock.Advance(time.Nanosecond)
	if _, err := r.Lookup(ctx, room); !errors.Is(err, errRoomExpired) {
		t.Fatalf("at boundary: %v", err)
	}
	if _, err := r.Visit(ctx, room); !errors.Is(err, errRoomExpired) {
		t.Fatalf("visit revived expired room: %v", err)
	}
	if _, err := r.BeginSession(ctx, room); !errors.Is(err, errRoomExpired) {
		t.Fatalf("WS revived expired room: %v", err)
	}
}

func TestRetentionProtectsSessionsAndPendingPersistence(t *testing.T) {
	clock := newTestClock()
	r, _ := policyRegistry(t, time.Hour, clock)
	ctx := context.Background()
	room, _ := r.Create(ctx)
	for range 2 {
		if _, err := r.BeginSession(ctx, room); err != nil {
			t.Fatal(err)
		}
	}
	r.Loaded(room)
	clock.Advance(30 * 24 * time.Hour)
	if n, err := r.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("active room swept: %d %v", n, err)
	}
	if err := r.EndSession(ctx, room, true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if n, err := r.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("other peer still active: %d %v", n, err)
	}
	if err := r.EndSession(ctx, room, true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if n, err := r.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("pending persistence swept: %d %v", n, err)
	}
	if err := r.Unloaded(ctx, room); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour - time.Nanosecond)
	if n, err := r.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("full TTL after unload: %d %v", n, err)
	}
	clock.Advance(time.Nanosecond)
	if n, err := r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("inactive room not swept: %d %v", n, err)
	}
}

func TestRetentionFailedUpgradeDoesNotRenew(t *testing.T) {
	clock := newTestClock()
	r, _ := policyRegistry(t, time.Hour, clock)
	ctx := context.Background()
	room, _ := r.Create(ctx)
	clock.Advance(30 * time.Minute)
	if _, err := r.BeginSession(ctx, room); err != nil {
		t.Fatal(err)
	}
	r.Loaded(room)
	if err := r.Unloaded(ctx, room); err != nil {
		t.Fatal(err)
	}
	if err := r.EndSession(ctx, room, false); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Minute)
	if n, err := r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("failed upgrade renewed TTL: %d %v", n, err)
	}
}

func TestRetentionPurgesAllCRDTDataAndKeepsTombstone(t *testing.T) {
	clock := newTestClock()
	r, path := policyRegistry(t, time.Hour, clock)
	ctx := context.Background()
	if err := r.Remember(ctx, "AAAAAA"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO updates VALUES ('AAAAAA', 1, X'0000', 0)`,
		`INSERT INTO snapshots VALUES ('AAAAAA', 'named', 1, X'0000')`,
		`INSERT INTO snapshot_versions (room, label, created_at, state) VALUES ('AAAAAA', 'saved', 0, X'0000')`,
		`INSERT INTO checkpoints VALUES ('AAAAAA', 1, NULL)`,
	} {
		if _, err := r.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(time.Hour)
	if n, err := r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep=%d %v", n, err)
	}
	for _, table := range []string{"updates", "snapshots", "snapshot_versions", "checkpoints"} {
		var count int
		if err := r.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE room = 'AAAAAA'").Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
	r.random = bytes.NewReader(append(bytes.Repeat([]byte{0}, 6), bytes.Repeat([]byte{1}, 6)...))
	if code, err := r.Create(ctx); err != nil || code != "BBBBBB" {
		t.Fatalf("retired code reused: %q %v", code, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := openRoomRegistry(path, rand.Reader, time.Hour, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, name := range []string{"AAAAAA", "aaaaaa"} {
		if _, err := r.BeginSession(ctx, name); !errors.Is(err, errRoomExpired) {
			t.Fatalf("reopened %s: %v", name, err)
		}
	}
}

func TestRetentionPurgeRollsBackOnFailure(t *testing.T) {
	clock := newTestClock()
	r, _ := policyRegistry(t, time.Hour, clock)
	ctx := context.Background()
	if err := r.Remember(ctx, "atomic"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO updates VALUES ('atomic', 1, X'0000', 0);
		INSERT INTO snapshots VALUES ('atomic', 'saved', 1, X'0000');
		CREATE TRIGGER fail_expiry BEFORE DELETE ON snapshots BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if _, err := r.Sweep(ctx); err == nil {
		t.Fatal("expected injected storage failure")
	}
	var count int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM updates WHERE room = 'atomic'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial purge: %d %v", count, err)
	}
	var expired sql.NullInt64
	if err := r.db.QueryRow(`SELECT expired_at FROM room_codes WHERE id = 'atomic'`).Scan(&expired); err != nil || expired.Valid {
		t.Fatalf("partial tombstone: %v %v", expired, err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER fail_expiry`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
}

func TestRetentionInactiveDeadlineSurvivesRestart(t *testing.T) {
	clock := newTestClock()
	r, path := policyRegistry(t, time.Hour, clock)
	ctx := context.Background()
	room, _ := r.Create(ctx)
	if _, err := r.BeginSession(ctx, room); err != nil {
		t.Fatal(err)
	}
	r.Loaded(room)
	if err := r.Unloaded(ctx, room); err != nil {
		t.Fatal(err)
	}
	if err := r.EndSession(ctx, room, true); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Minute)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRoomRegistry(path, rand.Reader, time.Hour, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	clock.Advance(30 * time.Minute)
	if _, err := reopened.Lookup(ctx, room); !errors.Is(err, errRoomExpired) {
		t.Fatalf("restart renewed inactive deadline: %v", err)
	}
}

func TestRetentionMigrationAndCrashRecovery(t *testing.T) {
	clock := newTestClock()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`CREATE TABLE room_codes (id TEXT PRIMARY KEY)`,
		`INSERT INTO room_codes VALUES ('LEGACY')`,
		`INSERT INTO updates VALUES ('fragment-room', 1, X'0000', 0)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	r, err := openRoomRegistry(path, rand.Reader, time.Hour, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, name := range []string{"LEGACY", "fragment-room"} {
		if got, err := r.Lookup(ctx, name); err != nil || got != name {
			t.Fatalf("migration %s: %q %v", name, got, err)
		}
	}
	if _, err := r.BeginSession(ctx, "LEGACY"); err != nil {
		t.Fatal(err)
	}
	// No EndSession: simulate an app crash with an active room.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * 24 * time.Hour)
	r, err = openRoomRegistry(path, rand.Reader, time.Hour, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("inactive legacy must expire: %d %v", n, err)
	}
	if got, err := r.Lookup(ctx, "LEGACY"); err != nil || got != "LEGACY" {
		t.Fatalf("active before crash lost: %q %v", got, err)
	}
	clock.Advance(time.Hour)
	if n, err := r.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("crash grace must eventually expire: %d %v", n, err)
	}
}

func TestRetentionSweepRacesWithSessionAdmission(t *testing.T) {
	clock := newTestClock()
	r, _ := policyRegistry(t, time.Hour, clock)
	ctx := context.Background()
	for range 30 {
		room, err := r.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Hour)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := r.Sweep(ctx)
			if err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := r.BeginSession(ctx, room); !errors.Is(err, errRoomExpired) {
				t.Errorf("expired room admitted: %v", err)
			}
		}()
		wg.Wait()
	}
}
