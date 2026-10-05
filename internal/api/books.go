package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gookorbit/internal/db"
	"gookorbit/internal/parser"
	"gookorbit/internal/storage"
)

const maxUploadBytes = 500 << 20 // 500 MB

var supportedFormats = map[string]bool{"epub": true, "pdf": true, "cbz": true}

type bookFileDTO struct {
	Format     string `json:"format"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	DownloadURL string `json:"download_url"`
}

type bookDTO struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Description    *string   `json:"description"`
	Language       string    `json:"language"`
	Publisher      *string   `json:"publisher"`
	PublishedDate  *string   `json:"published_date"`
	Authors        []string  `json:"authors"`
	Files          []bookFileDTO `json:"files"`
	CoverURL       *string   `json:"cover_url"`
	ProgressRatio  *float64  `json:"progress_ratio"`
	CurrentChapter *string   `json:"current_chapter"`
	IsFinished     bool      `json:"is_finished"`
	CreatedAt      time.Time `json:"created_at"`
}

// handleUploadBook: multipart upload -> sha256 + S3 (streamed) -> parse (epub)
// -> transactional DB insert.
func (s *Server) handleUploadBook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid, _ := userFromCtx(ctx)
	bookID := uuid.New()
	format := ""
	key := ""
	var rawFile *multipart.Part
	var rawName string
	var h = sha256.New()

	mr, err := r.MultipartReader()
	if err != nil {
		badRequest(w, fmt.Errorf("expected multipart form: %w", err))
		return
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			badRequest(w, err)
			return
		}
		if p.FormName() != "file" {
			_, _ = io.Copy(io.Discard, p)
			continue
		}
		rawName = p.FileName()
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(rawName)), ".")
		if !supportedFormats[ext] {
			badRequest(w, fmt.Errorf("unsupported format %q (epub, pdf, cbz)", ext))
			return
		}
		format = ext
		key = storage.BookKey(bookID, ext)
		rawFile = p
		break
	}
	if rawFile == nil {
		badRequest(w, fmt.Errorf("missing 'file' part"))
		return
	}
	defer rawFile.Close()

	// Stream to a disk temp file while computing sha256 (S3 Put and the
	// parser both want to re-read; memory stays flat). Bounded by
	// maxUploadBytes.
	tmp, err := os.CreateTemp("", "gookorbit-*.bin")
	if err != nil {
		s.log.Error("create temp", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer os.Remove(tmp.Name())
	// Tee each byte into the sha256 hasher; io.Copy writes them once to tmp.
	tee := io.TeeReader(io.LimitReader(rawFile, maxUploadBytes+1), h)
	size, err := io.Copy(tmp, tee)
	if err != nil {
		badRequest(w, fmt.Errorf("read upload: %w", err))
		return
	}
	if size > maxUploadBytes {
		badRequest(w, fmt.Errorf("file exceeds %d bytes", maxUploadBytes))
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	checksum := hex.EncodeToString(h.Sum(nil))

	// Parse metadata + cover for EPUB (pdf/cbz: no extraction).
	var meta parser.Metadata
	var cover []byte
	if format == "epub" {
		res, err := parser.Parse(tmp, size)
		if err != nil {
			badRequest(w, fmt.Errorf("parse epub: %w", err))
			return
		}
		meta = res.Meta
		cover = res.Cover
	}

	// Upload to S3 (stream from temp file).
	if err := s.store.PutObject(ctx, key, tmp, size, contentTypeFor(format)); err != nil {
		s.cleanupBook(ctx, bookID)
		s.log.Error("s3 put book", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "storage error"})
		return
	}

	// Cover upload.
	var coverKey pgtype.Text
	if len(cover) > 0 {
		ck := storage.CoverKey(bookID)
		if err := s.store.PutObject(ctx, ck, bytes.NewReader(cover), int64(len(cover)), "image/jpeg"); err != nil {
			s.log.Warn("cover upload failed", "err", err)
		} else {
			coverKey = pgtype.Text{String: ck, Valid: true}
		}
	}

	// DB transaction: book + authors + file.
	err = s.q.Transact(ctx, func(q *db.Queries) error {
		var pubDate pgtype.Date
		if !meta.PublishedDate.IsZero() {
			pubDate = pgtype.Date{Time: meta.PublishedDate, Valid: true}
		}
		book, err := q.InsertBook(ctx, db.InsertBookParams{
			ID:            bookID,
			Title:         meta.Title,
			Description:   pgtype.Text{String: meta.Description, Valid: meta.Description != ""},
			Language:      pgtype.Text{String: orDefault(meta.Language, "en"), Valid: true},
			Publisher:     pgtype.Text{String: meta.Publisher, Valid: meta.Publisher != ""},
			PublishedDate: pubDate,
		})
		if err != nil {
			return err
		}
		for _, name := range meta.Creators {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			a, err := q.GetOrCreateAuthor(ctx, name)
			if err != nil {
				return err
			}
			if err := q.InsertBookAuthor(ctx, db.InsertBookAuthorParams{BookID: book.ID, AuthorID: a.ID}); err != nil {
				return err
			}
		}
		_, err = q.InsertBookFile(ctx, db.InsertBookFileParams{
			BookID:         book.ID,
			Format:         format,
			S3Key:          key,
			FileSizeBytes:  size,
			Sha256Checksum: checksum,
		})
		if err != nil {
			return err
		}
		if coverKey.Valid {
			return q.SetCoverKey(ctx, db.SetCoverKeyParams{ID: book.ID, CoverS3Key: coverKey})
		}
		return nil
	})
	if err != nil {
		// Rollback S3 objects on DB failure.
		s.cleanupBook(ctx, bookID)
		s.log.Error("db insert book", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = uid
	w.Header().Set("Location", "/api/v1/books/"+bookID.String())
	writeJSON(w, http.StatusCreated, map[string]string{"id": bookID.String()})
}

func (s *Server) cleanupBook(ctx context.Context, bookID uuid.UUID) {
	if err := s.store.DeletePrefix(ctx, "books/"+bookID.String()+"/"); err != nil {
		s.log.Warn("cleanup failed", "book", bookID, "err", err)
	}
}

func contentTypeFor(format string) string {
	switch format {
	case "epub":
		return "application/epub+zip"
	case "pdf":
		return "application/pdf"
	case "cbz":
		return "application/vnd.comicbook+zip"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) handleListBooks(w http.ResponseWriter, r *http.Request) {
	uid, _ := userFromCtx(r.Context())
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 32)
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	page, _ := strconv.ParseInt(r.URL.Query().Get("page"), 10, 32)
	if page < 1 {
		page = 1
	}
	rows, err := s.q.ListBooks(r.Context(), db.ListBooksParams{
		UserID:  uid,
		Column2: r.URL.Query().Get("query"),
		Limit:   int32(limit),
		Offset:  int32((page - 1) * limit),
	})
	if err != nil {
		s.log.Error("list books", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	items := make([]bookDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, s.toDTO(row.ID, row.Title, row.Description, row.Language, row.Publisher, row.PublishedDate, row.CoverS3Key, row.Authors, row.Formats, row.Sizes, row.Checksums, row.S3Keys, row.ProgressRatio, row.CurrentChapter, row.IsFinished, row.CreatedAt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(items), "page": page, "items": items})
}

func (s *Server) handleGetBook(w http.ResponseWriter, r *http.Request) {
	uid, _ := userFromCtx(r.Context())
	id, ok := parseUUID(chi.URLParam(r, "id"))
	if !ok {
		badRequest(w, fmt.Errorf("invalid book id"))
		return
	}
	row, err := s.q.GetBook(r.Context(), db.GetBookParams{ID: id, UserID: uid})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "book not found"})
			return
		}
		s.log.Error("get book", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(row.ID, row.Title, row.Description, row.Language, row.Publisher, row.PublishedDate, row.CoverS3Key, row.Authors, row.Formats, row.Sizes, row.Checksums, row.S3Keys, row.ProgressRatio, row.CurrentChapter, row.IsFinished, row.CreatedAt))
}

func (s *Server) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(chi.URLParam(r, "id"))
	if !ok {
		badRequest(w, fmt.Errorf("invalid book id"))
		return
	}
	if err := s.q.DeleteBook(r.Context(), id); err != nil {
		s.log.Error("delete book", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := s.store.DeletePrefix(r.Context(), "books/"+id.String()+"/"); err != nil {
		s.log.Warn("s3 delete", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toDTO(id uuid.UUID, title string, desc pgtype.Text, lang pgtype.Text, pub pgtype.Text, pubDate pgtype.Date, cover pgtype.Text, authorsJSON interface{}, formats, sizes, checksums, s3keys interface{}, ratio pgtype.Numeric, chapter pgtype.Text, finished pgtype.Bool, createdAt pgtype.Timestamptz) bookDTO {
	var authors, fmts, sums []string
	var szs []int64
	var keys []string
	_ = json.Unmarshal([]byte(asString(authorsJSON)), &authors)
	_ = json.Unmarshal([]byte(asString(formats)), &fmts)
	_ = json.Unmarshal([]byte(asString(sizes)), &szs)
	_ = json.Unmarshal([]byte(asString(checksums)), &sums)
	_ = json.Unmarshal([]byte(asString(s3keys)), &keys)
	files := make([]bookFileDTO, 0, len(fmts))
	for i, f := range fmts {
		fe := bookFileDTO{Format: f, DownloadURL: "/api/v1/books/" + id.String() + "/download?format=" + f}
		if i < len(szs) {
			fe.SizeBytes = szs[i]
		}
		if i < len(sums) {
			fe.SHA256 = sums[i]
		}
		files = append(files, fe)
	}
	dto := bookDTO{
		ID:        id.String(),
		Title:     title,
		Language:  lang.String,
		Authors:   authors,
		Files:     files,
		IsFinished: finished.Bool,
	}
	if desc.Valid {
		dto.Description = &desc.String
	}
	if pub.Valid {
		dto.Publisher = &pub.String
	}
	if pubDate.Valid {
		d := pubDate.Time.Format("2006-01-02")
		dto.PublishedDate = &d
	}
	if cover.Valid {
		u := "/api/v1/books/" + id.String() + "/cover"
		dto.CoverURL = &u
	}
	if ratio.Valid {
		f, _ := ratio.Float64Value()
		dto.ProgressRatio = &f.Float64
	}
	if chapter.Valid {
		dto.CurrentChapter = &chapter.String
	}
	if createdAt.Valid {
		dto.CreatedAt = createdAt.Time
	}
	return dto
}

func asString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
