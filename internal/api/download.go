package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gookorbit/internal/db"
)

func (s *Server) handleBookCover(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(chi.URLParam(r, "id"))
	if !ok {
		badRequest(w, fmt.Errorf("invalid book id"))
		return
	}
	key, err := s.bookCoverKey(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no cover"})
		return
	}
	body, size, err := s.store.GetObject(r.Context(), key)
	if err != nil {
		s.log.Error("get cover", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "storage error"})
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(size))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (s *Server) handleBookDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(chi.URLParam(r, "id"))
	if !ok {
		badRequest(w, fmt.Errorf("invalid book id"))
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "epub"
	}
	var f db.BookFile
	{
		var err error
		f, err = s.q.GetBookFile(r.Context(), db.GetBookFileParams{BookID: id, Format: format})
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no file in this format"})
			return
		}
	}
	if s.store.PresignEnabled() {
		u, err := s.store.PresignURL(r.Context(), f.S3Key, 15*time.Minute)
		if err != nil {
			s.log.Error("presign", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "storage error"})
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	body, size, err := s.store.GetObject(r.Context(), f.S3Key)
	if err != nil {
		s.log.Error("get book", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "storage error"})
		return
	}
	defer body.Close()
	filename := fmt.Sprintf("book-%s.%s", id.String()[:8], format)
	ct := contentTypeFor(format)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(size))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (s *Server) bookCoverKey(ctx context.Context, id uuid.UUID) (string, error) {
	cover, err := s.q.GetBookCoverKey(ctx, id)
	if err != nil {
		return "", err
	}
	if !cover.Valid {
		return "", fmt.Errorf("no cover")
	}
	return cover.String, nil
}
