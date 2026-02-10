# DukeCam

Self-hosted construction photo documentation for Thomas Duke Company. Lightweight CompanyCam alternative.

## Stack

- **Backend:** Go + [Echo](https://echo.labstack.com/) — single binary, fast, minimal
- **Frontend:** [HTMX](https://htmx.org/) + [Tailwind CSS](https://tailwindcss.com/) — SPA feel, zero build step
- **Upload Engine:** Vanilla JS with IndexedDB queue — bulletproof offline-capable uploads
- **Database:** PostgreSQL
- **Image Processing:** Go `imaging` library — EXIF, thumbnails, auto-orientation
- **Deployment:** Docker on Framework via Cloudflare tunnel

## Features

- No auth required — workers pick name from dropdown, cookie remembers
- Bulletproof uploads — IndexedDB queue, auto-retry (10 attempts), works offline
- Day-grouped galleries — photos organized by date
- Share links — `/share/{slug}` for clean read-only galleries with OG meta
- QR codes — print and post at job sites
- EXIF extraction — GPS and timestamps
- Tags — Progress, Before, After, Issue
- HTMX admin — create projects/workers, toggle active, no page reloads

## Development

```bash
# Build and run locally (requires Go 1.23+)
go mod tidy
go run .

# Or use Docker
docker compose up --build
```

## Deployment

```bash
# Sync to Framework and rebuild
rsync -avz --delete --exclude .git \
  ~/claudecode/projects/dukecam/ framework-remote:~/dukecam/
ssh framework-remote "cd ~/dukecam && docker compose up -d --build"
```

## Architecture

```
dukecam/
├── main.go          # Entry point, config, router
├── db.go            # PostgreSQL queries (pgx)
├── pages.go         # Page handlers (home, project, share, admin)
├── api.go           # API handlers (upload, CRUD, QR)
├── media.go         # Photo/thumb serving (glob-based, fast)
├── imaging.go       # Image processing, EXIF, thumbnails
├── render.go        # Go template renderer
├── Dockerfile       # Multi-stage build (~15MB image)
├── docker-compose.yml
├── static/
│   ├── js/upload.js # IndexedDB upload engine
│   └── img/         # Logo, favicon
├── templates/
│   ├── base.html    # Layout (Tailwind + HTMX)
│   ├── home.html    # Project list
│   ├── project.html # Upload + gallery
│   ├── share.html   # Read-only gallery
│   ├── admin.html   # HTMX-powered admin
│   └── fragments.html # HTMX partial responses
└── comparison/      # "Why DukeCam?" page
```

## URL Routes

| Route | Description |
|-------|-------------|
| `GET /` | Project list |
| `GET /p/:slug` | Project page (upload + gallery) |
| `GET /share/:slug` | Shareable read-only gallery |
| `GET /admin` | Admin panel |
| `POST /api/upload` | Photo upload (JSON response) |
| `GET /media/photo/:slug/:file` | Full-size photo |
| `GET /media/thumb/:slug/:file` | Thumbnail |
