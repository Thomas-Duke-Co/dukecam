# DukeCam

Self-hosted construction photo documentation. A drop-in replacement for [CompanyCam](https://companycam.com) with **zero per-user fees**.

Built for field crews who need to quickly capture, organize, and share job site photos — without fumbling with logins or paying per seat.

## What It Does

- **Rapid Shoot camera** — open the viewfinder and snap photos as fast as you can tap. All photos queue and upload in the background, even on flaky cell signal.
- **Offline-first uploads** — photos persist in an IndexedDB queue that survives browser closes, signal drops, and page refreshes. Auto-retries with exponential backoff.
- **No logins** — workers pick their name from a dropdown. A cookie remembers it. No accounts, no passwords, no friction.
- **Project QR codes** — print a QR code, tape it to the job trailer. Anyone with a phone can start documenting.
- **Day-grouped timeline** — photos organized by date with worker initials on each tile.
- **Photo tagging** — tag photos as Progress, Before, After, or Issue. Edit tags and captions after upload.
- **Shareable gallery links** — send `/share/{project}` to clients or stakeholders. OG meta tags generate rich previews in iMessage, Slack, etc.
- **EXIF extraction** — GPS coordinates and timestamps pulled from photo metadata automatically.
- **Image auto-orientation** — EXIF orientation is applied server-side so photos always display correctly.
- **Lightbox with rotation** — browse photos with arrow keys or swipe on mobile. Rotate incorrectly-oriented photos with one tap.
- **PWA installable** — add to home screen on iOS/Android for a native app feel.

## Architecture

Single Go binary + PostgreSQL. No JavaScript build step. Templates are server-rendered HTML with Tailwind CSS (CDN) and vanilla JS.

```
dukecam (Go binary)
├── Echo HTTP server (:4010)
├── PostgreSQL (photos, projects, workers)
├── /data/photos/{slug}/{YYYY}/{MM}/{DD}/  ← full-size images
└── /data/thumbs/{slug}/{YYYY}/{MM}/{DD}/  ← 400×400 thumbnails
```

**Upload pipeline:**

```
Phone camera → IndexedDB queue → XHR upload (2 concurrent, 10 retries)
    ↓ server
Decode JPEG/PNG → Auto-orient via EXIF → Extract GPS/timestamp
    → Save full-size (quality 85)
    → Generate 400×400 Lanczos thumbnail
    → Store metadata in PostgreSQL
```

## Quick Start

### Docker Compose (recommended)

```yaml
services:
  dukecam:
    build: .
    container_name: dukecam
    restart: unless-stopped
    ports:
      - "4010:4010"
    environment:
      - DATABASE_URL=postgres://user:pass@db:5432/dukecam
      - STORAGE_PATH=/data/photos
      - THUMB_PATH=/data/thumbs
      - BASE_URL=https://photos.example.com
      - MAX_UPLOAD_MB=50
    volumes:
      - photo-data:/data

  db:
    image: postgres:16-alpine
    restart: unless-stopped
    environment:
      - POSTGRES_USER=user
      - POSTGRES_PASSWORD=pass
      - POSTGRES_DB=dukecam
    volumes:
      - pg-data:/var/lib/postgresql/data

volumes:
  photo-data:
  pg-data:
```

```bash
docker compose up -d
# Open http://localhost:4010
# Go to /admin to create your first project
```

### From source

```bash
# Requires Go 1.23+ and a PostgreSQL database
export DATABASE_URL="postgres://user:pass@localhost:5432/dukecam"
go build -o dukecam .
./dukecam
```

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://...@localhost:5432/dukecam` | PostgreSQL connection string |
| `STORAGE_PATH` | `/data/photos` | Where full-size photos are stored |
| `THUMB_PATH` | `/data/thumbs` | Where thumbnails are stored |
| `BASE_URL` | `https://dukecam.thomasduke.io` | Public URL (for QR codes and share links) |
| `MAX_UPLOAD_MB` | `50` | Maximum upload size per file |
| `PORT` | `4010` | HTTP server port |

Tables are created automatically on first run.

## API

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/upload` | Upload a photo (multipart form) |
| `GET` | `/api/photos/:slug` | List photos for a project (JSON, paginated) |
| `PATCH` | `/api/photo/:id` | Update caption and tag |
| `POST` | `/api/photo/:id/rotate` | Rotate a photo (`{"direction": "cw"/"ccw"/"180"}`) |
| `POST` | `/api/admin/project` | Create a project |
| `POST` | `/api/admin/worker` | Create a worker |
| `GET` | `/api/admin/project/:id/qr` | Download project QR code (PNG) |

## Pages

| Path | Description |
|------|-------------|
| `/` | Project list with photo counts and team bubbles |
| `/p/:slug` | Project page — upload photos + browse gallery |
| `/share/:slug` | Public share page — read-only gallery with OG tags |
| `/admin` | Admin panel — manage projects and workers |
| `/why` | CompanyCam cost comparison page |

## Project Structure

```
├── main.go          # Entry point, config, router
├── api.go           # Upload, CRUD, rotate, QR code endpoints
├── db.go            # PostgreSQL schema + queries (pgx)
├── pages.go         # Page handlers (home, project, share, admin)
├── media.go         # Photo/thumbnail serving
├── imaging.go       # EXIF extraction, auto-orientation, thumbnails, rotation
├── render.go        # Go template renderer
├── Dockerfile       # Multi-stage build (~15MB final image)
├── docker-compose.yml
├── static/
│   ├── js/upload.js # IndexedDB upload engine with offline support
│   ├── img/         # Logo, favicon
│   └── manifest.json
└── templates/
    ├── base.html    # Layout (Tailwind CDN + HTMX)
    ├── home.html    # Project list
    ├── project.html # Upload + gallery + rapid shoot camera
    ├── share.html   # Read-only gallery with OG meta
    ├── admin.html   # HTMX-powered admin panel
    └── fragments.html
```

## Tech Stack

- **[Go](https://go.dev)** with **[Echo](https://echo.labstack.com/)** — HTTP server, ~15MB Docker image
- **[PostgreSQL](https://postgresql.org)** via **[pgx](https://github.com/jackc/pgx)** — metadata storage
- **[disintegration/imaging](https://github.com/disintegration/imaging)** — image processing, auto-orientation, thumbnails
- **[rwcarlsen/goexif](https://github.com/rwcarlsen/goexif)** — GPS and timestamp extraction
- **[Tailwind CSS](https://tailwindcss.com)** (CDN) — styling with no build step
- **[HTMX](https://htmx.org)** — admin panel interactivity
- **Vanilla JS** — upload engine with IndexedDB queue

## Why Not CompanyCam?

CompanyCam charges **$37/user/month**. For a 14-person team, that's **$6,276/year** for features you can self-host for free.

DukeCam matches the core workflow — upload, organize, share — without the SaaS tax. No per-user fees means your team can grow without the bill growing with it.

## License

MIT
