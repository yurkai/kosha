package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
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

	"github.com/gorilla/websocket"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/persistence/sqlite"
	yws "github.com/reearth/ygo/provider/websocket"

	"kosha/web"
)

var validRoom = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type App struct {
	store         *sqlite.Store
	registry      *roomRegistryStore
	ws            *yws.Server
	handler       http.Handler
	rooms         atomic.Int64
	requests      atomic.Int64
	closeOnce     sync.Once
	closeErr      error
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}
}

func New(cfg Config) (*App, error) {
	return newApp(cfg, time.Now)
}

func newApp(cfg Config, now func() time.Time) (*App, error) {
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
	registry, err := openRoomRegistry(cfg.Database, rand.Reader, cfg.RoomTTL, now)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if _, err := registry.Sweep(context.Background()); err != nil {
		_ = registry.Close()
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
	ws.OnLoadDocument = func(_ context.Context, room string, _ *crdt.Doc) error {
		registry.Loaded(room)
		app.rooms.Add(1)
		return nil
	}
	ws.OnUnloadDocument = func(_ context.Context, room string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := registry.Unloaded(ctx, room); err != nil {
			slog.Error("room retention update failed", "error", err)
		}
		app.rooms.Add(-1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/rooms", app.createRoom)
	mux.HandleFunc("/api/rooms/{room}/visit", app.visitRoom)
	mux.HandleFunc("GET /api/rooms/{room}", app.roomStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("GET /yjs/{room}", func(w http.ResponseWriter, r *http.Request) {
		if !validRoom.MatchString(r.PathValue("room")) {
			http.Error(w, "invalid room name", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodGet || !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		name, err := registry.BeginSession(r.Context(), r.PathValue("room"))
		if err != nil {
			roomError(w, err)
			return
		}
		r.SetPathValue("room", name)
		tracked := &sessionWriter{ResponseWriter: w}
		app.requests.Add(1)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := registry.EndSession(ctx, name, tracked.upgraded); err != nil {
				slog.Error("room retention update failed", "error", err)
			}
			app.requests.Add(-1)
		}()
		ws.ServeHTTP(tracked, r)
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
		var canonical string
		var err error
		if r.Method == http.MethodGet {
			canonical, err = registry.Visit(r.Context(), name)
		} else {
			canonical, err = registry.Lookup(r.Context(), name)
		}
		if err != nil {
			roomError(w, err)
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
	cleanupCtx, cancel := context.WithCancel(context.Background())
	app.cleanupCancel, app.cleanupDone = cancel, make(chan struct{})
	go func() {
		defer close(app.cleanupDone)
		ticker := time.NewTicker(cleanupInterval(cfg.RoomTTL))
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-ticker.C:
				if count, err := registry.Sweep(cleanupCtx); err != nil {
					if cleanupCtx.Err() == nil {
						slog.Error("room cleanup failed", "error", err)
					}
				} else if count > 0 {
					slog.Info("expired inactive rooms", "count", count)
				}
			}
		}
	}()
	return app, nil
}

func (a *App) Handler() http.Handler { return a.handler }

func (a *App) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		a.registry.StopSessions()
		a.cleanupCancel()
		select {
		case <-a.cleanupDone:
		case <-ctx.Done():
			a.closeErr = ctx.Err()
			return
		}
		a.closeErr = a.ws.Shutdown(ctx)
		if err := a.registry.WaitSessions(ctx); err != nil {
			a.closeErr = errors.Join(a.closeErr, err)
			return // Keep stores open while a persistence drain is still in flight.
		}
		a.closeErr = errors.Join(a.closeErr, a.registry.Close(), a.store.Close())
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
	if !sameOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
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

type sessionWriter struct {
	http.ResponseWriter
	upgraded bool
}

func (w *sessionWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	w.upgraded = err == nil
	return conn, buffer, err
}

func (w *sessionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
}

func roomError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, errRoomExpired) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`<!doctype html><html lang="ru"><meta charset="utf-8"><title>Комната удалена — Kosha</title><h1>Срок хранения комнаты истёк</h1><p>Документ удалён после периода неактивности.</p><p><a href="/">Создать новую комнату</a></p></html>`))
		return
	}
	status := http.StatusInternalServerError
	if errors.Is(err, errServerClosing) {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, "room unavailable", status)
}

func (a *App) visitRoom(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	name := r.PathValue("room")
	if !validRoom.MatchString(name) {
		http.Error(w, "invalid room name", http.StatusBadRequest)
		return
	}
	name, err := a.registry.OpenLink(r.Context(), name)
	if err != nil {
		roomError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": name})
}

func (a *App) roomStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name := r.PathValue("room")
	if !validRoom.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	name, err := a.registry.Lookup(r.Context(), name)
	if err != nil {
		roomError(w, err)
		return
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": name})
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
