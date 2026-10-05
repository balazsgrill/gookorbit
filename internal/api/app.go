package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/google/uuid"

	"gookorbit/internal/config"
	"gookorbit/internal/db"
	"gookorbit/internal/koreader"
	"gookorbit/internal/storage"
)

type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	q       *db.Queries
	store   *storage.Service
	koreader *koreader.Generator
}

type userCtxKey struct{}

func New(cfg *config.Config, log *slog.Logger, q *db.Queries, store *storage.Service) *Server {
	return &Server{
		cfg:      cfg,
		log:      log,
		q:        q,
		store:    store,
		koreader: koreader.New(cfg.BaseURL),
	}
}

// ensureAdmin creates (or fetches) the built-in admin user on first use.
func (s *Server) ensureAdmin(ctx context.Context) (uuid.UUID, error) {
	u, err := s.q.GetOrCreateUser(ctx, db.GetOrCreateUserParams{
		Username:     "admin",
		PasswordHash: "gookorbit-env-token",
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("ensure admin user: %w", err)
	}
	return u.ID, nil
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger(s.log))
	r.Use(cors.AllowAll().Handler)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Admin API (token-authenticated, serves the web page + book management)
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.requireToken)
		r.Post("/books", s.handleUploadBook)
		r.Get("/books", s.handleListBooks)
		r.Get("/books/{id}", s.handleGetBook)
		r.Delete("/books/{id}", s.handleDeleteBook)
		r.Get("/books/{id}/cover", s.handleBookCover)
		r.Get("/books/{id}/download", s.handleBookDownload)
	})

	// KOReader native plugin API
	r.Route("/api/v1/koreader", func(r chi.Router) {
		r.Use(s.requireToken)
		r.Get("/plugin/download", s.handlePluginDownload)
		r.Get("/books", s.handleKoreaderBooks)
		r.Post("/sync/diff", s.handleSyncDiff)
		r.Put("/sync/progress", s.handleSyncProgress)
		r.Get("/books/{id}/download", s.handleBookDownload)
		r.Get("/books/{id}/cover", s.handleBookCover)
	})

	r.Get("/", s.serveIndex)
	return r
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			// Allow ?token= for plain link downloads (browser <a href>).
			got = r.URL.Query().Get("token")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.BootstrappedToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing bearer token"})
			return
		}
		ctx := r.Context()
		uid, err := s.ensureAdmin(ctx)
		if err != nil {
			s.log.Error("ensure admin", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, uid)))
	})
}

func userFromCtx(ctx context.Context) (uuid.UUID, error) {
	uid, ok := ctx.Value(userCtxKey{}).(uuid.UUID)
	if !ok {
		return uuid.Nil, errors.New("no user in context")
	}
	return uid, nil
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(ww, r)
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
