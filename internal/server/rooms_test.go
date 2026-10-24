package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence/sqlite"
)

func roomRegistry(t *testing.T, source io.Reader) (*roomRegistryStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rooms.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := openRoomRegistry(path, source, DefaultConfig().RoomTTL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry, path
}

func TestShortCodeCollisionAndPersistence(t *testing.T) {
	// AAAAAA is reserved, BBBBBB is a document from the previous version,
	// so the allocator must retry twice and return CCCCCC.
	stream := append(bytes.Repeat([]byte{0}, 6), bytes.Repeat([]byte{1}, 6)...)
	stream = append(stream, bytes.Repeat([]byte{2}, 6)...)
	r, path := roomRegistry(t, bytes.NewReader(stream))
	ctx := context.Background()
	if err := r.Remember(ctx, "aaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO updates (room, version, data, created_at) VALUES (?, 1, ?, 0)`, "BBBBBB", []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	code, err := r.Create(ctx)
	if err != nil || code != "CCCCCC" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRoomRegistry(path, rand.Reader, DefaultConfig().RoomTTL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, name := range []string{"aaaaaa", "BBBBBB", "CCCCCC"} {
		actual, err := reopened.Lookup(ctx, strings.ToLower(name))
		if err != nil || actual != name {
			t.Fatalf("lookup %s: %q %v", name, actual, err)
		}
	}
}

func TestShortCodeConcurrentAllocation(t *testing.T) {
	a, path := roomRegistry(t, rand.Reader)
	b, err := openRoomRegistry(path, rand.Reader, DefaultConfig().RoomTTL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	codes := make(chan string, 40)
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := a
			if i%2 == 1 {
				r = b
			}
			code, err := r.Create(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			codes <- code
		}(i)
	}
	wg.Wait()
	close(codes)
	seen := map[string]bool{}
	for code := range codes {
		if !regexp.MustCompile(`^[A-Z0-9]{6}$`).MatchString(code) || seen[code] {
			t.Fatalf("invalid or duplicated code %q", code)
		}
		seen[code] = true
	}
	if len(seen) != 40 {
		t.Fatalf("got %d codes", len(seen))
	}
}

func TestShortCodeEntropyFailure(t *testing.T) {
	r, _ := roomRegistry(t, strings.NewReader(""))
	if code, err := r.Create(context.Background()); err == nil || code != "" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestRoomCreationAndPageRoutes(t *testing.T) {
	app := testApp(t, false)
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/api/rooms", nil))
	var room struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 201 || !regexp.MustCompile(`^[A-Z0-9]{6}$`).MatchString(room.ID) || room.Path != "/"+room.ID || rr.Header().Get("Location") != room.Path {
		t.Fatalf("bad creation response: %d %s", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		method, path, origin string
		want                 int
	}{
		{"GET", room.Path, "", 200},
		{"HEAD", room.Path, "", 200},
		{"GET", "/NOTFOUND", "", 404},
		{"GET", "/api/rooms", "", 405},
		{"POST", "/api/rooms", "https://unrelated.example", 403},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		app.Handler().ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, rr.Code, tc.want)
		}
	}
	if err := app.registry.Remember(context.Background(), "Q1W2E3"); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/q1w2e3", nil))
	if rr.Code != 308 || rr.Header().Get("Location") != "/Q1W2E3" {
		t.Fatalf("canonical URL: %d %s", rr.Code, rr.Body.String())
	}
}
