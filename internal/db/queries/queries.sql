-- name: GetOrCreateUser :one
INSERT INTO users (username, password_hash) VALUES ($1, $2)
ON CONFLICT (username) DO UPDATE SET updated_at = NOW()
RETURNING id, username;

-- name: GetOrCreateAuthor :one
INSERT INTO authors (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id, name;

-- name: InsertBook :one
INSERT INTO books (id, title, description, language, publisher, published_date)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: SetCoverKey :exec
UPDATE books SET cover_s3_key = $2, updated_at = NOW() WHERE id = $1;

-- name: InsertBookFile :one
INSERT INTO book_files (book_id, format, s3_key, file_size_bytes, sha256_checksum)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: InsertBookAuthor :exec
INSERT INTO book_authors (book_id, author_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: InsertAPIToken :exec
INSERT INTO api_tokens (user_id, name, token_hash) VALUES ($1, $2, $3)
ON CONFLICT (token_hash) DO NOTHING;

-- name: DeleteBook :exec
DELETE FROM books WHERE id = $1;

-- name: GetBookFile :one
SELECT * FROM book_files WHERE book_id = $1 AND format = $2;

-- name: GetBookFileByChecksum :one
SELECT bf.id, bf.book_id, bf.format, bf.s3_key, bf.file_size_bytes, bf.sha256_checksum, b.title
FROM book_files bf
JOIN books b ON b.id = bf.book_id
WHERE bf.sha256_checksum = $1;

-- name: ListPendingFiles :many
SELECT bf.id, bf.book_id, bf.format, bf.s3_key, bf.file_size_bytes, bf.sha256_checksum, b.title
FROM book_files bf
JOIN books b ON b.id = bf.book_id
WHERE NOT (bf.sha256_checksum = ANY($1::text[]))
ORDER BY b.created_at DESC;

-- name: ListBooks :many
SELECT
    b.id,
    b.title,
    b.description,
    b.language,
    b.publisher,
    b.published_date,
    b.cover_s3_key,
    b.created_at,
    COALESCE((json_agg(a.name) FILTER (WHERE a.id IS NOT NULL))::text, '[]') AS authors,
    COALESCE((json_agg(bf.format ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS formats,
    COALESCE((json_agg(bf.file_size_bytes ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS sizes,
    COALESCE((json_agg(bf.sha256_checksum ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS checksums,
    COALESCE((json_agg(bf.s3_key ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS s3_keys,
    u.progress_ratio,
    u.current_chapter,
    u.is_finished,
    u.updated_at AS progress_updated_at
FROM books b
LEFT JOIN book_authors ba ON ba.book_id = b.id
LEFT JOIN authors a ON a.id = ba.author_id
LEFT JOIN book_files bf ON bf.book_id = b.id
LEFT JOIN user_book_progress u ON u.book_id = b.id AND u.user_id = $1
WHERE ($2::text IS NULL
       OR b.title ILIKE '%' || $2 || '%'
       OR a.name ILIKE '%' || $2 || '%')
GROUP BY b.id, u.progress_ratio, u.current_chapter, u.is_finished, u.updated_at
ORDER BY b.created_at DESC
LIMIT $3 OFFSET $4;

-- name: GetBookCoverKey :one
SELECT cover_s3_key FROM books WHERE id = $1;

-- name: GetBook :one
SELECT
    b.id,
    b.title,
    b.description,
    b.language,
    b.publisher,
    b.published_date,
    b.cover_s3_key,
    b.created_at,
    COALESCE((json_agg(a.name) FILTER (WHERE a.id IS NOT NULL))::text, '[]') AS authors,
    COALESCE((json_agg(bf.format ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS formats,
    COALESCE((json_agg(bf.file_size_bytes ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS sizes,
    COALESCE((json_agg(bf.sha256_checksum ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS checksums,
    COALESCE((json_agg(bf.s3_key ORDER BY bf.format) FILTER (WHERE bf.id IS NOT NULL))::text, '[]') AS s3_keys,
    u.progress_ratio,
    u.current_chapter,
    u.is_finished,
    u.updated_at AS progress_updated_at
FROM books b
LEFT JOIN book_authors ba ON ba.book_id = b.id
LEFT JOIN authors a ON a.id = ba.author_id
LEFT JOIN book_files bf ON bf.book_id = b.id
LEFT JOIN user_book_progress u ON u.book_id = b.id AND u.user_id = $2
WHERE b.id = $1
GROUP BY b.id, u.progress_ratio, u.current_chapter, u.is_finished, u.updated_at;

-- name: GetProgressForUpdate :one
SELECT * FROM user_book_progress
WHERE user_id = $1 AND book_id = $2
FOR UPDATE;

-- name: UpdateProgress :one
UPDATE user_book_progress
SET progress_ratio = $3,
    current_chapter = $4,
    progress_data = $5,
    is_finished = $6,
    updated_at = NOW()
WHERE user_id = $1 AND book_id = $2
RETURNING *;

-- name: InsertProgress :one
INSERT INTO user_book_progress (user_id, book_id, progress_ratio, current_chapter, progress_data, is_finished)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;
