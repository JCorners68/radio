# Personal Radio Station

A lightweight, self-hosted personal radio station with a warm analog-broadcast aesthetic. Curate playlists ("crates"), stream tracks via an embedded YouTube player with live Web Audio visualizations, and optionally use LLMs to discover songs and build custom crates.

No frontend build step, no bundler, no node_modules — just vanilla HTML/CSS/JS served by a small Go backend.

---

## Features

- **The Booth (`/`)** — Main broadcast console with:
  - Embedded YouTube player + real-time dual-mode visualizer (32-bar frequency analyzer & oscilloscope)
  - Multiple independent playlists ("crates") with shuffle, autopilot continuous play, and inline track ratings (1–5 stars)
  - Live filter, sort (index, title, artist, length, rating, play count), and inline metadata editing
  - JSON playlist export/import and point-in-time server snapshots
- **Find (`/find.html`)** — Natural-language track discovery:
  - Describe a mood, artist, era, or obscure theme ("late-night 70s Krautrock motorik", "upbeat Japanese city pop")
  - Uses an LLM to suggest tracks, resolves each candidate to a playable YouTube video via `yt-dlp`, and streams results back live over SSE
  - Preview inline or add selected tracks directly to any crate
- **AI Crate Builder (`/ai-builder.html`)** — Conversational playlist curation:
  - Chat with an AI DJ to brainstorm, refine, and batch-resolve whole playlists

---

## Quick Start

### Prerequisites

- **Go 1.24+**
- **`yt-dlp`** (installed on your `PATH` or placed at `./bin/yt-dlp`) — used to resolve YouTube URLs and metadata
- *(Optional)* Google Cloud / Vertex AI credentials or a local OpenAI-compatible LLM endpoint for AI features

### Run

```bash
make run
```

Then open **[http://localhost:8765](http://localhost:8765)** in your browser.

> **Note:** The station booth, playlist management, and YouTube URL/ID lookup work out of the box without any AI credentials configured.

---

## AI Configuration (Optional)

To enable **Find** and **AI Crate Builder**, copy `.env.example` to `.env` and configure your preferred model provider:

```bash
cp .env.example .env
```

```dotenv
# Google Cloud / Vertex AI (used by AI Builder & Find w/ Gemini)
GOOGLE_CLOUD_PROJECT=your-gcp-project-id
GOOGLE_CLOUD_LOCATION=global

# Optional local OpenAI-compatible endpoint (used by Find w/ local Qwen)
QWEN_URL=http://localhost:8000/v1
QWEN_MODEL=qwen3.8-27b
```

If using Vertex AI, authenticate once with Application Default Credentials:

```bash
gcloud auth application-default login
```

---

## Project Structure

```text
.
├── index.html          # Main radio station booth
├── find.html           # Natural-language track finder (SSE streaming)
├── ai-builder.html     # Conversational AI crate builder
├── station-store.js    # Shared client state & localStorage/server sync
├── DESIGN.md           # Detailed architecture & API documentation
├── Makefile            # Build, run, and test targets
└── aiserver/           # Go HTTP server (:8765)
    ├── main.go         # Static file server, env loader, routes
    ├── store.go        # Atomic JSON persistence & rolling snapshots
    ├── find.go         # SSE track discovery pipeline & title scoring
    ├── vertex.go       # Vertex AI Gemini integration
    ├── youtube.go      # yt-dlp metadata & search wrapper
    └── server_test.go  # Unit test suite
```

All user playlists and snapshots are stored locally in `./data/` (gitignored).

---

## Development

```bash
make test    # Run Go unit tests
make build   # Compile ./aiserver/radio-ai-server
make run     # Build and start server on http://localhost:8765
```

For deeper details on the state synchronization model, scoring heuristics, and API endpoints, see [DESIGN.md](DESIGN.md).
