package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func retentionApp(t *testing.T, clock *testClock) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Database = filepath.Join(t.TempDir(), "http.db")
	cfg.RoomTTL, cfg.Metrics = time.Hour, true
	app, err := newApp(cfg, clock.Now)
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
	return app
}

func requestStatus(app *App, method, path string) int {
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return rr.Code
}

func TestRetentionHTTPVisitsAndTechnicalRequests(t *testing.T) {
	for _, methodAndPath := range []struct {
		method, path string
		renews       bool
	}{
		{"GET", "/%s", true}, {"POST", "/api/rooms/%s/visit", true},
		{"HEAD", "/%s", false}, {"GET", "/api/rooms/%s", false},
		{"GET", "/healthz", false}, {"GET", "/debug/metrics", false},
		{"GET", "/yjs/%s", false},
	} {
		t.Run(methodAndPath.method+methodAndPath.path, func(t *testing.T) {
			clock := newTestClock()
			app := retentionApp(t, clock)
			room, err := app.registry.Create(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			clock.Advance(30 * time.Minute)
			path := strings.ReplaceAll(methodAndPath.path, "%s", room)
			status := requestStatus(app, methodAndPath.method, path)
			if path != "/yjs/"+room && status != 200 {
				t.Fatalf("request %s: %d", path, status)
			}
			clock.Advance(30 * time.Minute)
			want := http.StatusGone
			if methodAndPath.renews {
				want = http.StatusOK
			}
			if got := requestStatus(app, "GET", "/api/rooms/"+room); got != want {
				t.Fatalf("at original deadline: %d want %d", got, want)
			}
		})
	}
}

func TestRetentionRealWebSocketsAndFinalLeave(t *testing.T) {
	clock := newTestClock()
	app := retentionApp(t, clock)
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	room, _ := app.registry.Create(context.Background())
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/yjs/" + room
	var peers []*websocket.Conn
	defer func() {
		for _, peer := range peers {
			_ = peer.Close()
		}
	}()
	for range 2 {
		peer, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, peer)
	}
	clock.Advance(40 * 24 * time.Hour)
	if n, err := app.registry.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("active room expired: %d %v", n, err)
	}
	_ = peers[0].Close()
	waitUntil(t, func() bool { return app.requests.Load() == 1 })
	clock.Advance(2 * time.Hour)
	if n, err := app.registry.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("remaining peer lost room: %d %v", n, err)
	}
	_ = peers[1].Close()
	waitUntil(t, func() bool { return app.requests.Load() == 0 && app.rooms.Load() == 0 })
	clock.Advance(time.Hour - time.Nanosecond)
	if got := requestStatus(app, "GET", "/api/rooms/"+room); got != 200 {
		t.Fatalf("room expired before full TTL: %d", got)
	}
	clock.Advance(time.Nanosecond)
	if got := requestStatus(app, "GET", "/"+room); got != 410 {
		t.Fatalf("expired page: %d", got)
	}
	peer, response, err := websocket.DefaultDialer.Dial(url, nil)
	if peer != nil {
		peer.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != 410 {
		t.Fatalf("expired WS admitted: %v %v", response, err)
	}
}

func TestRetentionLegacyOpenAndExpiredAliases(t *testing.T) {
	clock := newTestClock()
	app := retentionApp(t, clock)
	if got := requestStatus(app, "POST", "/api/rooms/old-fragment/visit"); got != 200 {
		t.Fatalf("legacy creation: %d", got)
	}
	clock.Advance(time.Hour)
	if got := requestStatus(app, "POST", "/api/rooms/old-fragment/visit"); got != 410 {
		t.Fatalf("legacy resurrected: %d", got)
	}
	if got := requestStatus(app, "POST", "/api/rooms/OLD-FRAGMENT/visit"); got != 410 {
		t.Fatalf("legacy case alias resurrected: %d", got)
	}
	if got := requestStatus(app, "GET", "/never-existed"); got != 404 {
		t.Fatalf("unknown page: %d", got)
	}
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for server state")
		}
		time.Sleep(time.Millisecond)
	}
}
