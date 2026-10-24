package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/persistence/sqlite"
	yws "github.com/reearth/ygo/provider/websocket"

	"kosha/web"
)

var validRoom = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type App struct {
	store     *sqlite.Store
	ws        *yws.Server
	handler   http.Handler
	rooms     atomic.Int64
	requests  atomic.Int64
	closeOnce sync.Once
	closeErr  error
}

func New(cfg Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Database), 0700); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(cfg.Database)
	if err != nil {
		return nil, err
	}
	adapter := persistence.NewLegacyAdapter(store)
	adapter.KeepVersions = 32
	ws := yws.NewServerWithPersistence(adapter)
	ws.MaxRooms = cfg.MaxRooms
	ws.MaxPeersPerRoom = cfg.MaxPeers
	ws.MaxConnections = cfg.MaxRooms * cfg.MaxPeers
	ws.MaxMessageBytes = cfg.MaxMessageBytes
	ws.MaxUpdateBytes = int(cfg.MaxMessageBytes)
	ws.MaxPendingItems = 4096
	ws.MaxAwarenessBytesPerRoom = 64 << 10
	ws.MaxAwarenessClientsPerRoom = 32
	ws.PeerWriteQueueSize = 32
	ws.CompactEvery = 32
	ws.PersistCoalesceWindow = 250 * time.Millisecond
	ws.PersistCoalesceMaxWait = time.Second
	app := &App{store: store, ws: ws}
	ws.OnLoadDocument = func(_ context.Context, _ string, _ *crdt.Doc) error {
		app.rooms.Add(1)
		return nil
	}
	ws.OnUnloadDocument = func(_ context.Context, _ string) { app.rooms.Add(-1) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("GET /yjs/{room}", func(w http.ResponseWriter, r *http.Request) {
		if !validRoom.MatchString(r.PathValue("room")) {
			http.Error(w, "invalid room name", http.StatusBadRequest)
			return
		}
		app.requests.Add(1)
		defer app.requests.Add(-1)
		ws.ServeHTTP(w, r)
	})
	if cfg.Metrics {
		mux.HandleFunc("GET /debug/metrics", app.metrics)
	}
	mux.Handle("GET /", http.FileServer(http.FS(web.Assets)))
	app.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/" || r.URL.Path == "/debug/metrics" || r.URL.Path == "/healthz" {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }

func (a *App) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		a.closeErr = errors.Join(a.ws.Shutdown(ctx), a.store.Close())
	})
	return a.closeErr
}

func (a *App) metrics(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"heap_alloc_bytes": m.HeapAlloc, "heap_inuse_bytes": m.HeapInuse,
		"go_sys_bytes": m.Sys, "gc_cycles": m.NumGC,
		"goroutines": runtime.NumGoroutine(), "resident_rooms": a.rooms.Load(),
		"websocket_handlers": a.requests.Load(),
	})
}
