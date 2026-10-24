package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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
	registry  *roomRegistryStore
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
	registry, err := openRoomRegistry(cfg.Database, rand.Reader)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	index, err := fs.ReadFile(web.Assets, "index.html")
	if err != nil {
		_ = registry.Close()
		_ = store.Close()
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
	app := &App{store: store, registry: registry, ws: ws}
	ws.OnLoadDocument = func(ctx context.Context, room string, _ *crdt.Doc) error {
		if err := registry.Remember(ctx, room); err != nil {
			return err
		}
		app.rooms.Add(1)
		return nil
	}
	ws.OnUnloadDocument = func(_ context.Context, _ string) { app.rooms.Add(-1) }

	mux := http.NewServeMux()
	mux.HandleFunc("/api/rooms", app.createRoom)
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
	mux.HandleFunc("GET /{room}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("room")
		if !validRoom.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		canonical, err := registry.Lookup(r.Context(), name)
		if err != nil {
			http.Error(w, "room unavailable", http.StatusInternalServerError)
			return
		}
		if canonical == "" {
			http.NotFound(w, r)
			return
		}
		if canonical != name {
			http.Redirect(w, r, "/"+canonical, http.StatusPermanentRedirect)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
	files := http.FileServer(http.FS(web.Assets))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	})
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
		a.closeErr = errors.Join(a.ws.Shutdown(ctx), a.registry.Close(), a.store.Close())
	})
	return a.closeErr
}

func (a *App) createRoom(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
	}
	code, err := a.registry.Create(r.Context())
	if err != nil {
		http.Error(w, "could not create room", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/"+code)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": code, "path": "/" + code})
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
