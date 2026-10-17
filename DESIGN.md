# Radio Station — design

Personal, non-commercial YouTube music player for headset time while coding. One Go process
serves three pages and a small API on `http://localhost:8420`. It has no accounts and no cloud
hosting, and it is reachable only from this machine.

**Last updated:** 2026-10-16.

---

## 1. Run it

```bash
make run            # builds aiserver/radio-ai-server and serves http://127.0.0.1:8420
```

Open **`http://localhost:8420`**. Use `localhost`, not `127.0.0.1`: the browser treats them as
different sites, and any browser-side storage is keyed to `localhost:8420` (§9).

Local machine settings can be placed in a gitignored `<root>/.env` file or passed as environment
variables:

| env var | default | what |
|---|---|---|
| `RADIO_LISTEN` | `127.0.0.1:8420` | listen address. Keep it on loopback because there is no auth. |
| `RADIO_ROOT` | parent of the binary's dir | where the pages live |
| `RADIO_DATA` | `<root>/data` | where `station.json` and backups live |
| `RADIO_ENV_FILE` | `<root>/.env` | dotenv file read for `GOOGLE_SERVICE_ACCOUNT_JSON` (§6) |
| `YTDLP` | `<root>/bin/yt-dlp`, else `$PATH` | yt-dlp binary |
| `QWEN_URL` | `http://127.0.0.1:8000/v1` | OpenAI-compatible Qwen endpoint |
| `QWEN_MODEL` | `qwen3.8-27b` | |

At startup the server checks for yt-dlp, the pages and the service account, and refuses to start
with a clear message if one is missing.

---

## 2. Layout

```
radio-station/
├── index.html          the player: playlists, YouTube iframe, autopilot, import/export
├── ai-builder.html     ✨ AI Suggest: seed → Gemini picks → real YouTube videos
├── find.html           🔎 Copy & Find: Spotify list → YouTube matches (direct or Qwen)
├── station-store.js    shared client storage module used by all three pages (§3)
├── Makefile            build / run / test
├── bin/yt-dlp          standalone yt-dlp release binary (no Python, gitignored)
├── data/               gitignored local library
│   ├── station.json    THE library: {rev, savedAt, state}
│   └── backups/        station-YYYY-MM-DD.json (30 kept)
└── aiserver/           Go module `radio-ai-server`
    ├── main.go         startup, routes, page serving, Gemini call + top-up loop
    ├── store.go        server-owned storage: atomic writes, revisions, daily backups
    ├── youtube.go      yt-dlp search, URL-seed resolution, the pick→video matcher
    ├── find.go         Copy & Find: Spotify embed parsing, direct tier, Qwen agent, NDJSON
    ├── vertex.go       minimal Vertex AI client (service account → OAuth token)
    └── server_test.go  unit tests for matcher, parsers and store lifecycle
```

The server exposes only the three pages, `station-store.js`, `/healthz` and `/api/*`. Every
other path returns 404, including `data/`, the source and `bin/`.

---

## 3. Storage

```
page load ──GET /index.html──► server injects window.__STATION__ = {rev, state}  (no fetch wait)
edit      ──PUT /api/state {rev, state}──► rev matches?  yes → atomic write, rev+1, 200 {rev}
                                                         no  → 409, tab adopts the server copy
other tabs ◄── BroadcastChannel('station') {rev} ── refetch GET /api/state
```

- **`data/station.json`** is the only source of truth. Each write goes to a temp file, is
  fsynced, and is renamed into place, so a crash cannot leave a torn file.
- **Revisions.** Every write must carry the rev it was based on. A stale rev gets 409, so the
  player tab and an AI-builder tab can't silently overwrite each other.
- **Validation.** The server refuses a state without `playlists[]` and `tracks{}`, so a page bug
  cannot wipe the library.
- **Backups.** The first write of each day copies the previous file into
  `data/backups/station-YYYY-MM-DD.json`, and only the newest 30 are kept. Restoring one means
  stopping the server, copying the backup over `data/station.json`, and starting it again.
- **Two client write styles** (`station-store.js`):
  - `Station.save(state)`: a debounced (300 ms) whole-state PUT. The player uses it because it
    mutates constantly (play counts, volume). On 409 it adopts the server state but keeps its own
    playback fields (`adoptServerState` in `index.html`). An in-flight local change can be lost
    in that race, which is acceptable for a personal app.
  - `Station.update(fn)`: fetches the latest state, applies `fn`, PUTs, and retries on 409. The
    AI builder and Copy & Find use it because they make discrete changes that must not be lost.
- **Tab close.** `pagehide` sends any pending save with `keepalive`. Browsers cap keepalive
  bodies at 64 KB, so a very large library relies on the 300 ms debounce having already flushed.

**State shape** (unchanged from the localStorage era, so `station.json` exports and imports
still work):

```js
{
  playlists: [ {id, name, trackIds: [trackId, ...]} ],
  tracks: { trackId: {id, videoId, title, thumb, favorite, playCount, addedAt, autopilot?} },
  activePlaylistId, currentTrackId, shuffle, repeat, autopilot, volume, showVideo, videoMaximized
}
```

Tracks are deduplicated by `videoId` across playlists. A playlist only holds ids.

---

## 4. The player (`index.html`)

- **Playback** uses the YouTube iframe API. Media Session keys (headset buttons) map to
  play/pause/next/prev.
- **Shuffle** is weighted by `log(playCount)`, with favourites at ×2.5, and never repeats the track
  that just played.
- **Autopilot:** when a non-shuffled playlist runs out, it seeds `/api/suggest` with the current
  track, favourites and the most-played tracks, then appends up to 8 new tracks.
- **Adding a track:** paste a YouTube URL. The title and thumbnail come from YouTube oEmbed in the
  browser.
- **Export/import:** `station.json` (the whole state), and per-playlist `.m3u` / `.txt` URL lists
  that work with `yt-dlp -a`.
- **Per-track ✨ button:** opens `ai-builder.html` seeded with that track's YouTube URL.

---

## 5. ✨ AI Suggest (`POST /api/suggest {seed, count}`)

```
seed ──► resolveSeed: a YouTube URL becomes "the song in this video: <title> (channel: <ch>)" via yt-dlp
     ──► Gemini 3.6 Flash (Vertex, JSON mode, no tools): N picks of {title, artist, reason}
     ──► for each pick, in parallel: ytsearch5 → matcher (§7) → accept the best hit or drop the pick
     ──► top-up loop: up to 4 rounds, asking for ~2× the shortfall, with already-picked titles excluded
```

The split is deliberate:
- **Gemini picks the songs, which it does well.** It is never trusted for video IDs: when it was
  asked for them, roughly half were invented, even with search grounding.
- **Gemini can't open URLs on this path.** That is why `resolveSeed` exists. Before 2026-10-16 a
  URL seed (what the per-track ✨ button sends) was an opaque ID to the model: "Thunderstruck" by
  AC/DC came back as post-hardcore recommendations.

---

## 6. External dependencies

| dependency | used for | notes |
|---|---|---|
| Vertex AI, `gemini-3.6-flash` | AI Suggest picks | Service account JSON comes from `GOOGLE_SERVICE_ACCOUNT_JSON` in `RADIO_ENV_FILE`. It is read with a line scan, never `source`d, never logged, never copied. |
| yt-dlp (`bin/yt-dlp`) | every YouTube search and URL lookup | Standalone release binary. Update it with `bin/yt-dlp -U` when searches start failing, because YouTube changes break old versions. |
| Qwen3.8-27B (`QWEN_URL`) | Copy & Find hard cases | vLLM, OpenAI-compatible, tool calling on. If the Qwen server is down, hard tracks report "Qwen unavailable" and the direct tier still works. |
| Spotify embed pages (`open.spotify.com/embed/...`) | reading pasted playlists, albums and tracks | Public, no key. The data is parsed out of the page's `__NEXT_DATA__` JSON, so it **breaks if Spotify changes that page**; the error says so. Tested up to 50 tracks; a very long playlist may be truncated by Spotify. |
| YouTube oEmbed, iframe API, `i.ytimg.com` | browser side: titles, playback, thumbnails | |

---

## 7. The matcher (`youtube.go`: `scoreHit` / `bestHit`)

This is a deterministic check that a YouTube hit really is the requested recording. Both AI
Suggest and Copy & Find depend on it.

1. **Normalise:** lowercase, strip accents (`Âme` → `ame`), drop punctuation, and turn `&` into
   `and`. Bracketed qualifiers such as `(feat. X)` and `[Remastered]` are removed from the
   requested title.
2. **Hard gate:** at least 60% of the title's words must appear in the hit's title, and at least
   50% of the artist's words in the hit's title + channel. Run-together channel names such as
   `ACDCVEVO` are matched as substrings.
3. **Score:** `2×title + artist`, then:
   - −1.5 per off-recording word (cover, karaoke, reaction, slowed, nightcore, 1 hour, loop, …)
     unless the requested title contains it;
   - +0.3 for official, `- Topic` and VEVO uploads;
   - when the length is known: +1.0 within 5 s, +0.4 within 20 s, −1.0 beyond 90 s;
   - when the length is unknown: −1.0 for anything over 15 minutes (a mix, not a track).

Before this existed, hit #1 was taken blindly: "Anatomy" resolved to a Grey's Anatomy clip, and
"Âme" to an unrelated A-Wall video.

---

## 8. 🔎 Copy & Find (`POST /api/find {text}` → NDJSON stream)

**Input:** Spotify playlist, album or track links, `spotify:` URIs, or free-text lines. Free text
is split on ` - ` / ` – ` / ` — ` as a guess at "Artist – Title". Duplicates are removed, and a
run is capped at 300 tracks.

**Per track, run concurrently (6 direct workers, 3 Qwen workers):**

```
tier 1  direct: ytsearch8 "<artist> <title>" → matcher
        accept if the matcher passes AND (|len − spotify len| ≤ 5 s, or no length known and a full match)
tier 2  qwen:   Qwen3.8 + a youtube_search tool. It sees the target and the tier-1 hits,
                runs up to 5 more searches, then answers {"videoId", "confidence", "reason"}
        Go rejects any videoId that never appeared in a real search result.
        An empty or unparseable answer (usually finish=length, the reasoning ran out of budget)
        gets one retry with thinking OFF, asking for the JSON only.
```

**Stream events**, one JSON object per line:
- `{"type":"source","name","tracks":[{title,artist,secs,raw?}]}` once, first;
- `{"type":"result","i","found","method":"direct"|"qwen","videoId","title","channel","secs","thumb","confidence","reason","searches"}`
  per track, arriving **in completion order** (use `i` to place each one);
- `{"type":"done","found","total"}` at the end.

`find.html` renders rows as they arrive. Low-confidence matches start unticked. Saving goes
through `Station.update`, to a new playlist named after the Spotify source or to an existing one.

**Measured 2026-10-16:**

| run | found | how | time |
|---|---|---|---|
| *OK Computer* album + 3 free-text lines | 15/15 | all direct, lengths within 1 s | 4.8 s |
| "Today's Top Hits", 50 tracks | 49/50 | 49 direct; the miss was the budget problem, since fixed | 26 s |
| misspelled or descriptive lines | 3/3 | Qwen | ~20 s |

Examples from the Qwen runs:
- It worked out that Green Velvet is also Cajmere, which is how "Perkulator" found "Percolator".
- It fixed "Boards of Canda".
- It identified "the song from the opening of Drive" as Kavinsky's "Nightcall".

---

## 9. History and gotchas

- **Consolidated into a single process on 2026-10-16.** Previously the page was served on :8420
  by a separate static server, with the API on :8421 behind CORS, and yt-dlp ran from a Python
  venv. All of that is now one Go process on :8420 with paths set by `.env` / environment
  variables and a standalone `yt-dlp` binary.
- **Playlists used to live in the browser** (`localStorage`, key `station_v1`, site
  `http://localhost:8420`). They were migrated to `data/station.json` on the server.
- **Spotify Web API support was removed.** It was a "find this YouTube track on Spotify" button
  that never worked, because no client credentials were ever configured. Copy & Find goes the
  other way and needs no credentials.
- **Qwen3.8 is a reasoning model.** On an unfamiliar track it can spend the whole token budget
  thinking. That is why the budget is 4096 tokens and why there is a thinking-off retry.

## 10. Known gaps

- AI Suggest doesn't tell Gemini what is already in the library, so it can suggest songs you
  already have. The client skips exact `videoId` duplicates only.
- Unit tests (`make test`, `aiserver/server_test.go`) cover the deterministic matcher, URL/JSON
  parsers and `Store` lifecycle; the live AI/yt-dlp endpoints are integration-tested by hand.
- It is a single process with no supervisor. Run it in tmux, or add a user systemd unit if it
  should start on login.
- There is no auth. It is safe only because it binds to loopback, so don't change `RADIO_LISTEN`
  to `0.0.0.0`.
