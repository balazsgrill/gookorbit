# `gookorbit`: System Architecture & Technical Specification

## 1. Project Overview & System Philosophy

`gookorbit` is a lightweight, single-binary reimplementation of the BookOrbit backend written in Go. It provides book cataloging, metadata extraction, S3-backed asset management, and a dedicated native integration layer for KOReader e-ink devices.

```
                    +------------------------------------------+
                    |           Client Layer                   |
                    |  [KOReader Device]     [Web / REST API]  |
                    +---------+--------------------+-----------+
                              |                    |
                              | HTTP / JSON        |
                              v                    v
+----------------------------------------------------------------------+
|                         gookorbit Daemon                             |
|                                                                      |
|  +------------------------+  +------------------------------------+  |
|  | KOReader Protocol Hub  |  | Ingestion & Extraction Worker      |  |
|  | - Pre-seeded Zip Pack  |  | - Streaming EPUB/PDF parser        |  |
|  | - Catalog / Queue API  |  | - Cover generator                  |  |
|  | - 2-Way Progress Sync  |  | - SHA256 integrity check          |  |
|  +-----------+------------+  +-----------------+------------------+  |
|              |                                 |                     |
|  +-----------v---------------------------------v------------------+  |
|  | Service Layer (Catalog, Users, Sync, Storage Broker)            |  |
|  +-------------------+-------------------------+------------------+  |
+----------------------|-------------------------|---------------------+
                       | SQL queries             | S3 Put/Get / Presign
                       v                         v
       +-----------------------+     +-----------------------+
       |   PostgreSQL 15+      |     |  S3-Compatible Store  |
       |  (Relational Index &  |     |  (MinIO, Garage, R2,  |
       |   pg_trgm Search)     |     |   Ceph Object Store)  |
       +-----------------------+     +-----------------------+

```

### Architectural Principles

* **Stateless Binary:** The application retains zero local disk state. All configuration is read from environment variables; all binary book blobs live in S3; all metadata and progress states live in PostgreSQL.
* **Low Memory Footprint:** Max idle footprint target is under **30 MB RSS**. Under active streaming ingestion, memory must stay strictly under **100 MB RSS** via streamed buffers.
* **Presigned S3 Transfers:** Raw book downloads by devices can optionally bypass the Go application process entirely via direct S3 presigned URLs, eliminating bandwidth bottlenecks on low-spec hosts.
* **Frictionless E-Ink Onboarding:** Provide a single-click pre-configured KOReader plugin generator containing instance URLs and scoped auth tokens.

---

## 2. Technology Stack & Dependencies

| Component | Selected Tool / Library | Justification |
| --- | --- | --- |
| **Language** | Go 1.23+ | Native concurrency, minimal resource usage, single static binary compilation. |
| **Router** | `[github.com/go-chi/chi/v5](https://github.com/go-chi/chi/v5)` | Idiomatic, zero-allocation routing fully compatible with standard `net/http`. |
| **Database Driver** | `[github.com/jackc/pgx/v5](https://github.com/jackc/pgx/v5)` | High-performance PostgreSQL native driver with connection pooling. |
| **Data Access** | `sqlc` | Compile-time type-safe Go code generation from raw SQL; avoids ORM reflection overhead. |
| **Migrations** | `golang-migrate/migrate/v4` | Automated, embedded forward/backward database migrations on startup. |
| **Object Storage** | `[github.com/aws/aws-sdk-go-v2/service/s3](https://github.com/aws/aws-sdk-go-v2/service/s3)` | Universal S3 API support (AWS S3, MinIO, Garage, Cloudflare R2). |
| **Document Parsers** | EPUB: Custom lightweight zip/XML parser; PDF: streaming header/XMP metadata parser. | Native parsing without external runtime dependencies (no Calibre/Python CLI). |
| **Search Engine** | PostgreSQL Full-Text Search + `pg_trgm` | Sub-millisecond fuzzy search without vector databases or Elasticsearch. |

---

## 3. Database Schema (PostgreSQL)

Execute via `sqlc`-compatible migration files.

```sql
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    username VARCHAR(64) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE api_tokens (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(64) NOT NULL,
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE books (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    title VARCHAR(512) NOT NULL,
    description TEXT,
    language VARCHAR(16) DEFAULT 'en',
    publisher VARCHAR(256),
    published_date DATE,
    cover_s3_key VARCHAR(1024),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE authors (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name VARCHAR(256) UNIQUE NOT NULL
);

CREATE TABLE book_authors (
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    author_id UUID NOT NULL REFERENCES authors(id) ON DELETE CASCADE,
    PRIMARY KEY (book_id, author_id)
);

CREATE TABLE book_files (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    format VARCHAR(16) NOT NULL, -- 'epub', 'pdf', 'cbz'
    s3_key VARCHAR(1024) NOT NULL,
    file_size_bytes BIGINT NOT NULL,
    sha256_checksum VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT unique_book_format UNIQUE (book_id, format)
);

CREATE TABLE user_book_progress (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    progress_ratio NUMERIC(5, 4) NOT NULL DEFAULT 0.0000, -- 0.0000 to 1.0000
    current_chapter VARCHAR(256),
    progress_data JSONB NOT NULL DEFAULT '{}', -- Holds KOReader xpointer/CFI/epoch info
    is_finished BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, book_id)
);

CREATE TABLE bookmarks_annotations (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    book_id UUID NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    highlight_text TEXT,
    note TEXT,
    location_marker VARCHAR(512) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Search Indexes
CREATE INDEX idx_books_title_trgm ON books USING gin (title gin_trgm_ops);
CREATE INDEX idx_authors_name_trgm ON authors USING gin (name gin_trgm_ops);
CREATE INDEX idx_book_files_checksum ON book_files(sha256_checksum);

```

---

## 4. S3 Storage Architecture

### Object Key Hierarchy

Objects are organized in a single S3 bucket using deterministic prefixes:

```
books/
  ├── {book_uuid}/
  │     ├── original.{format}     # e.g., books/123e4567.../original.epub
  │     └── cover.jpg             # Extracted cover image (WebP or JPEG)

```

### Storage Operations Interface

```go
type StorageService interface {
    UploadBook(ctx context.Context, bookID uuid.UUID, format string, r io.Reader, size int64) (s3Key string, err error)
    UploadCover(ctx context.Context, bookID uuid.UUID, r io.Reader, contentType string) (s3Key string, err error)
    GetPresignedDownloadURL(ctx context.Context, s3Key string, expiry time.Duration) (string, error)
    StreamObject(ctx context.Context, s3Key string) (io.ReadCloser, int64, error)
    DeleteBookAssets(ctx context.Context, bookID uuid.UUID) error
}

```

*Direct Download Fallback:* If the client or deployment environment restricts public presigned S3 URLs (e.g. S3 instance on an internal Docker network inaccessible to the e-reader), `gookorbit` falls back to direct proxy streaming (`io.Copy` to `http.ResponseWriter`) without loading the full file into memory.

---

## 5. Metadata Ingestion Pipeline

To avoid external dependencies like `calibre` or Python tools:

1. **Upload Handler:** Accepts multipart book upload (`POST /api/v1/books`).
2. **Streaming Sha256 Calculation:** As bytes stream into an `io.TeeReader`, compute SHA256 checksum and stream directly into S3.
3. **EPUB Unpack & Parse (in-memory zip reader):**
* Reads the `container.xml` to locate `content.opf`.
* Parses Dublin Core XML nodes (`dc:title`, `dc:creator`, `dc:language`, `dc:description`, `dc:date`).
* Locates `<item id="cover-image" ...>` from the manifest, streams out the binary cover, resizes to max width 600px using a pure Go imaging library (`[github.com/disintegration/imaging](https://github.com/disintegration/imaging)`), and stores to `books/{id}/cover.jpg`.


4. **Database Record Creation:** Inserts rows across `books`, `authors`, and `book_files` in a single transaction.

---

## 6. KOReader Integration Specification

KOReader does not need to navigate the OPDS catalog. It communicates with `gookorbit` via a specialized native plugin (`gookorbit.koplugin`).

### 6.1 The Plugin Auto-Provisioning Engine

The user accesses the Web UI on desktop and clicks **"Download KOReader Plugin"**.

**Endpoint:** `GET /api/v1/koreader/plugin/download`

The backend generates an in-memory zip archive on the fly:

```
gookorbit.koplugin/
  ├── _meta.lua
  ├── main.lua
  └── config.lua

```

The server generates a scoped long-lived API token, reads the host URL from the incoming request or configuration, and compiles `config.lua`:

```lua
-- Auto-generated by gookorbit
return {
    base_url = "http://192.168.1.100:8080",
    api_token = "gko_tok_9b2d8f1e7a4c...",
    sync_interval_seconds = 60,
    download_directory = "/mnt/onboard/books"
}

```

The user unpacks this folder into KOReader's `koreader/plugins/` directory.

---

### 6.2 KOReader API Endpoints

All endpoints require standard authorization: `Authorization: Bearer <api_token>`.

#### 1. Catalog Browsing & Search

```http
GET /api/v1/koreader/books?page=1&limit=25&query=tchaikovsky

```

**Response (200 OK):**

```json
{
  "total": 1,
  "page": 1,
  "items": [
    {
      "id": "7f0980cf-f8c7-43a0-8d54-8e10228bbce2",
      "title": "Children of Time",
      "authors": ["Adrian Tchaikovsky"],
      "format": "epub",
      "size_bytes": 1540301,
      "published_date": "2015-06-04",
      "progress_ratio": 0.4210,
      "cover_url": "/api/v1/koreader/books/7f0980cf-f8c7-43a0-8d54-8e10228bbce2/cover"
    }
  ]
}

```

#### 2. The "Not on Device" Queue Diff

KOReader gathers local file checksums or filenames and posts them to receive pending server books:

```http
POST /api/v1/koreader/sync/diff
Content-Type: application/json

{
  "known_checksums": [
    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
  ]
}

```

**Response (200 OK):** Returns items present on the server whose `sha256_checksum` is not in `known_checksums`.

#### 3. Book Retrieval (Binary / Presign redirect)

```http
GET /api/v1/koreader/books/{id}/download

```

* **Behavior:** If `STORAGE_PRESIGN=true`, returns `302 Found` with S3 presigned URL. Otherwise, streams raw binary data directly with `Content-Type: application/epub+zip` and `Content-Disposition: attachment; filename="..."`.

#### 4. Bidirectional Progress Sync

```http
PUT /api/v1/koreader/sync/progress
Content-Type: application/json

{
  "book_id": "7f0980cf-f8c7-43a0-8d54-8e10228bbce2",
  "progress_ratio": 0.4520,
  "current_chapter": "Chapter 12: Contact",
  "progress_data": {
    "xpointer": "/6/14[chapter-12]!/4/2/1:0",
    "device_timestamp": 1726051200
  }
}

```

* **Conflict Resolution Logic:** The server uses last-write-wins based on epoch timestamp. If `device_timestamp` is older than the database `updated_at`, the server returns `409 Conflict` containing the newer server state:

```json
{
  "conflict": true,
  "server_progress": {
    "progress_ratio": 0.5100,
    "current_chapter": "Chapter 14",
    "updated_at": "2026-09-25T10:14:00Z"
  }
}

```

---

## 7. Configuration Specification

Configuration must load cleanly from environment variables via a minimal parser (e.g. `[github.com/caarlos0/env/v11](https://github.com/caarlos0/env/v11)`):

```bash
# Server Core
PORT=8080
LOG_LEVEL=info # debug, info, warn, error
BASE_URL=http://192.168.1.100:8080

# Database
DATABASE_URL=postgres://gookorbit:secret@localhost:5432/gookorbit?sslmode=disable&pool_max_conns=10

# S3 Object Storage
S3_ENDPOINT=minio.homelab.local:9000
S3_REGION=us-east-1
S3_BUCKET=gookorbit-library
S3_ACCESS_KEY=admin
S3_SECRET_KEY=password123
S3_USE_SSL=false
S3_FORCE_PATH_STYLE=true
STORAGE_PRESIGN=false # Set to true to bypass backend when serving downloads

```

---

## 8. Step-by-Step AI Agent Implementation Roadmap

This plan provides unambiguous instructions for a coding agent to execute in sequential steps.

### Step 1: Project Setup & Database Foundations

1. Initialize Go module: `go mod init [github.com/username/gookorbit](https://github.com/username/gookorbit)`.
2. Setup directories:
* `cmd/server/main.go`
* `internal/config/`
* `internal/database/migrations/`
* `internal/database/queries/`
* `internal/storage/`
* `internal/parser/`
* `internal/api/`
* `internal/koreader/`


3. Write SQL migration scripts conforming to Section 3.
4. Configure `sqlc.yaml` and generate Go models and queriers for CRUD on `books`, `authors`, and `user_book_progress`.

### Step 2: Storage Broker & Ingestion Parser

1. Implement `internal/storage/s3.go` using AWS SDK v2, supporting `PutObject`, `GetObject`, and `PresignGetObject`.
2. Implement `internal/parser/epub.go`:
* Accept an `io.ReaderAt` or temporary file reader.
* Open `zip.Reader`, extract metadata elements from `.opf`.
* Extract and return cover bytes.



### Step 3: REST API & Routing

1. Set up `chi` router in `internal/api/routes.go` with structured logging middleware using `log/slog`.
2. Implement authentication middleware validating `Authorization: Bearer <token>` against `api_tokens`.
3. Add Book CRUD endpoints:
* `POST /api/v1/books` (Multipart upload -> Parser -> S3 -> DB).
* `GET /api/v1/books` (List books with trigram search filter).
* `GET /api/v1/books/{id}`.
* `DELETE /api/v1/books/{id}` (Deletes DB record and queues S3 asset deletion).



### Step 4: KOReader Plugin Generator & API

1. Embed the template files for `gookorbit.koplugin` (`main.lua`, `_meta.lua`) into the Go binary using `//go:embed`.
2. Implement `GET /api/v1/koreader/plugin/download` to dynamically inject instance configuration into `config.lua` and stream out a `.zip`.
3. Implement `GET /api/v1/koreader/books` and `PUT /api/v1/koreader/sync/progress`.

### Step 5: Verification & Containerization

1. Build a multi-stage `Dockerfile`:
* Builder: `golang:1.23-alpine` (CGO_ENABLED=0).
* Runner: `scratch` or `alpine:3.20` with `ca-certificates`.


2. Total container image size: **< 25 MB**.
3. Verify memory utilization under load using `docker stats` to ensure it operates within target constraints.