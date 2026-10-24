package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Address != "127.0.0.1:8080" || cfg.MaxRooms != 5 || cfg.MaxPeers != 5 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Database = ":memory:" },
		func(c *Config) { c.Database = "" },
		func(c *Config) { c.MaxRooms = 0 },
		func(c *Config) { c.MaxPeers = 0 },
		func(c *Config) { c.MaxMessageBytes = 0 },
	} {
		bad := cfg
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid configuration accepted: %+v", bad)
		}
	}
}

func testApp(t *testing.T, metrics bool) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Database = filepath.Join(t.TempDir(), "nested", "test.db")
	cfg.Metrics = metrics
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return app
}

func TestHTTPRoutes(t *testing.T) {
	app := testApp(t, false)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/healthz", 200},
		{"POST", "/healthz", 405},
		{"GET", "/", 200},
		{"GET", "/missing", 404},
		{"GET", "/debug/metrics", 404},
		{"GET", "/yjs/invalid!", 400},
		{"GET", "/yjs/" + strings.Repeat("a", 65), 400},
		{"POST", "/yjs/valid-room", 405},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			app.Handler().ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != tc.status {
				t.Fatalf("status = %d; want %d; body=%s", rr.Code, tc.status, rr.Body.String())
			}
			if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing nosniff header")
			}
		})
	}
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body, _ := io.ReadAll(rr.Result().Body)
	if !strings.Contains(string(body), "Kosha") {
		t.Fatal("embedded frontend was not served")
	}
}

func TestMetricsAreOptIn(t *testing.T) {
	app := testApp(t, true)
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/metrics", nil))
	var m struct {
		HeapAlloc uint64 `json:"heap_alloc_bytes"`
		Rooms     int64  `json:"resident_rooms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || m.HeapAlloc == 0 || m.Rooms != 0 {
		t.Fatalf("unexpected metrics: %s", rr.Body.String())
	}
}

func TestWebSocketLimitsOriginAndRoomRelease(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Database = filepath.Join(t.TempDir(), "test.db")
	cfg.MaxRooms, cfg.MaxPeers = 1, 1
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	httpServer := httptest.NewServer(app.Handler())
	defer httpServer.Close()
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/yjs/"
	conn, _, err := websocket.DefaultDialer.Dial(url+"first", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, room := range []string{"first", "second"} {
		extra, response, err := websocket.DefaultDialer.Dial(url+room, nil)
		if extra != nil {
			extra.Close()
		}
		if response != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("limit not enforced for %s: response=%v, err=%v", room, response, err)
		}
	}
	_ = conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for app.rooms.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if app.rooms.Load() != 0 {
		t.Fatal("room retained after final participant left")
	}
	conn, response, err := websocket.DefaultDialer.Dial(url+"origin", http.Header{"Origin": {"https://unrelated.example"}})
	if conn != nil {
		conn.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin socket accepted: response=%v, err=%v", response, err)
	}
}
