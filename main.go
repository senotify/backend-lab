package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	db    *DB
	cache resultCache
	sweep atomic.Pointer[Sweep]
	rl    *rateLimiter
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := OpenDB(ctx)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{db: db, rl: newRateLimiter(30, time.Minute)}

	go func() {
		if err := db.Setup(ctx); err != nil {
			db.state.Store("setup failed: " + err.Error())
			log.Println("setup failed:", err)
			return
		}
		log.Println("ready")
		s.refreshSweep(ctx)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshSweep(ctx)
			}
		}
	}()

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Println("listening on :8080")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (s *server) refreshSweep(ctx context.Context) {
	start := time.Now()
	sw, err := s.db.RunSweep(ctx)
	if err != nil {
		log.Println("sweep failed:", err)
		return
	}
	s.sweep.Store(sw)
	log.Printf("sweep measured in %s", time.Since(start).Round(time.Millisecond))
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(web))

	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(web, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", page("index.html"))
	mux.HandleFunc("GET /pagination", page("pagination.html"))
	mux.Handle("GET /static/", static) // files live at web/static/ in the embed
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /api/\nSitemap: https://lab.senotify.com/sitemap.xml\n"))
	})
	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://lab.senotify.com/</loc></url><url><loc>https://lab.senotify.com/pagination</loc></url></urlset>`))
	})

	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/pagination/strategies", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Strategies) })
	mux.HandleFunc("GET /api/pagination/run", s.run)
	mux.HandleFunc("GET /api/pagination/rows", s.rows)
	mux.HandleFunc("GET /api/pagination/sweep", func(w http.ResponseWriter, r *http.Request) {
		sw := s.sweep.Load()
		if sw == nil {
			writeJSON(w, 503, map[string]string{"error": "still measuring"})
			return
		}
		writeJSON(w, 200, sw)
	})
	return securityHeaders(mux)
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ready": s.db.ready.Load(), "state": s.db.state.Load(),
		"totalRows": TotalRows, "totalPages": TotalPages, "pageSize": PageSize, "checkpointStep": CheckpointStep,
	})
}

func parsePage(r *http.Request) (int, bool) {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	return p, err == nil && p >= 1 && p <= TotalPages
}

func (s *server) run(w http.ResponseWriter, r *http.Request) {
	if !s.db.ready.Load() {
		writeJSON(w, 503, map[string]any{"error": s.db.state.Load()})
		return
	}
	strategy := r.URL.Query().Get("strategy")
	if _, ok := strategyByKey(strategy); !ok {
		writeJSON(w, 400, map[string]string{"error": "unknown strategy"})
		return
	}
	page, ok := parsePage(r)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "page must be 1-" + strconv.Itoa(TotalPages)})
		return
	}
	key := strategy + ":" + strconv.Itoa(page)
	if res, ok := s.cache.get(key); ok {
		res.Cached = true
		writeJSON(w, 200, res)
		return
	}
	if !s.rl.allow(clientIP(r)) {
		writeJSON(w, 429, map[string]string{"error": "Easy there: 30 runs a minute. The database is shared with real apps."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	res, err := s.db.Run(ctx, strategy, page)
	if err != nil {
		log.Println("run:", err)
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	if !res.TimedOut {
		s.cache.put(key, *res)
	}
	writeJSON(w, 200, res)
}

func (s *server) rows(w http.ResponseWriter, r *http.Request) {
	if !s.db.ready.Load() {
		writeJSON(w, 503, map[string]any{"error": s.db.state.Load()})
		return
	}
	page, ok := parsePage(r)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "bad page"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	rows, err := s.db.Rows(ctx, page)
	if err != nil {
		log.Println("rows:", err)
		writeJSON(w, 500, map[string]string{"error": "query failed"})
		return
	}
	writeJSON(w, 200, rows)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.ServeHTTP(w, r)
	})
}

// clientIP trusts the last X-Forwarded-For hop, which Caddy sets.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*bucket
}
type bucket struct {
	n     int
	reset time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: window, hits: map[string]*bucket{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.hits[key]
	if !ok || now.After(b.reset) {
		if len(l.hits) > 10000 {
			l.hits = map[string]*bucket{}
		}
		l.hits[key] = &bucket{1, now.Add(l.window)}
		return true
	}
	b.n++
	return b.n <= l.max
}
