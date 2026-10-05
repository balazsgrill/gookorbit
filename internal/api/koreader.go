package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gookorbit/internal/db"
	"gookorbit/internal/koreader"
)

type koreaderBookDTO struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Authors       []string `json:"authors"`
	Format        string   `json:"format"`
	SizeBytes     int64    `json:"size_bytes"`
	PublishedDate *string  `json:"published_date"`
	ProgressRatio *float64 `json:"progress_ratio"`
	CoverURL      string   `json:"cover_url"`
	DownloadURL   string   `json:"download_url"`
	Checksum      string   `json:"checksum"`
}

func (s *Server) handleKoreaderBooks(w http.ResponseWriter, r *http.Request) {
	uid, _ := userFromCtx(r.Context())
	page, _ := strconv.ParseInt(r.URL.Query().Get("page"), 10, 32)
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 32)
	if limit < 1 || limit > 100 {
		limit = 25
	}
	query := r.URL.Query().Get("query")
	rows, err := s.q.ListBooks(r.Context(), db.ListBooksParams{
		UserID:  uid,
		Column2: query,
		Limit:   int32(limit),
		Offset:  int32((page - 1) * limit),
	})
	if err != nil {
		s.log.Error("koreader list", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	items := make([]koreaderBookDTO, 0, len(rows))
	for _, row := range rows {
		var fmts, sums []string
		var szs []int64
		_ = json.Unmarshal([]byte(asString(row.Formats)), &fmts)
		_ = json.Unmarshal([]byte(asString(row.Sizes)), &szs)
		_ = json.Unmarshal([]byte(asString(row.Checksums)), &sums)
		if len(fmts) == 0 {
			continue // book with no files is not downloadable
		}
		format := fmts[0]
		var authors []string
		_ = json.Unmarshal([]byte(asString(row.Authors)), &authors)
		b := koreaderBookDTO{
			ID:          row.ID.String(),
			Title:       row.Title,
			Authors:     authors,
			Format:      format,
			CoverURL:    "/api/v1/koreader/books/" + row.ID.String() + "/cover",
			DownloadURL: "/api/v1/koreader/books/" + row.ID.String() + "/download",
		}
		if len(szs) > 0 {
			b.SizeBytes = szs[0]
		}
		if len(sums) > 0 {
			b.Checksum = sums[0]
		}
		if row.PublishedDate.Valid {
			d := row.PublishedDate.Time.Format("2006-01-02")
			b.PublishedDate = &d
		}
		if row.ProgressRatio.Valid {
			f, _ := row.ProgressRatio.Float64Value()
			b.ProgressRatio = &f.Float64
		}
		items = append(items, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(items),
		"page":  page,
		"items": items,
	})
}

func (s *Server) handleSyncDiff(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KnownChecksums []string `json:"known_checksums"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, fmt.Errorf("invalid json: %w", err))
		return
	}
	if req.KnownChecksums == nil {
		req.KnownChecksums = []string{}
	}
	rows, err := s.q.ListPendingFiles(r.Context(), req.KnownChecksums)
	if err != nil {
		s.log.Error("sync diff", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	items := make([]koreaderBookDTO, 0, len(rows))
	seen := map[uuid.UUID]bool{}
	for _, row := range rows {
		if seen[row.BookID] {
			continue
		}
		seen[row.BookID] = true
		items = append(items, koreaderBookDTO{
			ID:          row.BookID.String(),
			Title:       row.Title,
			Authors:     []string{},
			Format:      row.Format,
			SizeBytes:   row.FileSizeBytes,
			CoverURL:    "/api/v1/koreader/books/" + row.BookID.String() + "/cover",
			DownloadURL: "/api/v1/koreader/books/" + row.BookID.String() + "/download",
			Checksum:    row.Sha256Checksum,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": items})
}

type progressRequest struct {
	BookID         string          `json:"book_id"`
	ProgressRatio  float64         `json:"progress_ratio"`
	CurrentChapter string          `json:"current_chapter"`
	ProgressData   json.RawMessage `json:"progress_data"`
}

func (s *Server) handleSyncProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req progressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, fmt.Errorf("invalid json: %w", err))
		return
	}
	bookID, ok := parseUUID(req.BookID)
	if !ok {
		badRequest(w, fmt.Errorf("invalid book_id"))
		return
	}
	if req.ProgressRatio < 0 || req.ProgressRatio > 1 {
		badRequest(w, fmt.Errorf("progress_ratio must be in [0,1]"))
		return
	}
	if req.ProgressData == nil {
		req.ProgressData = json.RawMessage("{}")
	}
	uid, _ := userFromCtx(ctx)

	// Last-write-wins on the epoch carried in progress_data (spec's
	// device_timestamp); the stored epoch is kept in progress_data.epoch.
	var deviceTS int64
	_ = json.Unmarshal(req.ProgressData, &struct {
		DeviceTimestamp *int64 `json:"device_timestamp"`
	}{DeviceTimestamp: &deviceTS})
	if deviceTS == 0 {
		badRequest(w, fmt.Errorf("progress_data.device_timestamp (unix epoch) is required"))
		return
	}

	err := s.q.Transact(ctx, func(q *db.Queries) error {
		cur, err := q.GetProgressForUpdate(ctx, db.GetProgressForUpdateParams{
			UserID: uid,
			BookID: bookID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			var stored struct {
				Epoch *int64 `json:"epoch"`
			}
			_ = json.Unmarshal(cur.ProgressData, &stored)
			if stored.Epoch != nil && *stored.Epoch > deviceTS {
				return errConflict{p: cur}
			}
		}
		data, mErr := mergeEpoch(req.ProgressData, deviceTS)
		if mErr != nil {
			return mErr
		}
		chapter := pgtype.Text{String: req.CurrentChapter, Valid: req.CurrentChapter != ""}
		var ratio pgtype.Numeric
		if err := ratio.Scan(strconv.FormatFloat(req.ProgressRatio, 'f', 4, 64)); err != nil {
			return err
		}
		if err == pgx.ErrNoRows {
			_, err = q.InsertProgress(ctx, db.InsertProgressParams{
				UserID:         uid,
				BookID:         bookID,
				ProgressRatio:  ratio,
				CurrentChapter: chapter,
				ProgressData:   data,
				IsFinished:     req.ProgressRatio >= 1,
			})
		} else {
			_, err = q.UpdateProgress(ctx, db.UpdateProgressParams{
				UserID:         uid,
				BookID:         bookID,
				ProgressRatio:  ratio,
				CurrentChapter: chapter,
				ProgressData:   data,
				IsFinished:     req.ProgressRatio >= 1,
			})
		}
		return err
	})
	if err != nil {
		var ce errConflict
		if errors.As(err, &ce) {
			ratio, _ := ce.p.ProgressRatio.Float64Value()
			writeJSON(w, http.StatusConflict, map[string]any{
				"conflict": true,
				"server_progress": map[string]any{
					"progress_ratio":  ratio.Float64,
					"current_chapter": ce.p.CurrentChapter.String,
					"updated_at":      ce.p.UpdatedAt.Time.UTC().Format(time.RFC3339),
				},
			})
			return
		}
		s.log.Error("sync progress", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "synced"})
}

// mergeEpoch stamps the epoch into progress_data for future comparisons.
func mergeEpoch(data json.RawMessage, ts int64) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		m = map[string]any{}
	}
	m["epoch"] = ts
	return json.Marshal(m)
}

type errConflict struct{ p db.UserBookProgress }

func (e errConflict) Error() string { return "progress conflict" }

func (s *Server) handlePluginDownload(w http.ResponseWriter, r *http.Request) {
	uid, _ := userFromCtx(r.Context())
	zdata, token, err := s.koreader.PluginZip()
	if err != nil {
		s.log.Error("plugin zip", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := s.q.InsertAPIToken(r.Context(), db.InsertAPITokenParams{
		UserID:    uid,
		Name:      "koreader-" + time.Now().Format("20060102-150405"),
		TokenHash: koreader.TokenHash(token),
	}); err != nil {
		s.log.Error("insert device token", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="gookorbit.koplugin.zip"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(zdata)
}
