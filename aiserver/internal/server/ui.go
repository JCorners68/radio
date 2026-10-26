package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"radio-ai-server/internal/auth"
	"radio-ai-server/internal/store"
)

type uiViewData struct {
	User           auth.User
	IsRemote       bool
	State          store.StationState
	ActivePlaylist store.Playlist
	OtherPlaylists []store.Playlist
	Tracks         []store.Track
	CurrentTrack   *store.Track
	NextTrack      *store.Track
	InitialTrackID string
	InitialVideoID string
	ShowVideo      bool
}

type trackRowData struct {
	Track          store.Track
	CurrentTrackID string
	ActivePlaylist store.Playlist
	OtherPlaylists []store.Playlist
}

func makeTrackRow(t store.Track, currentTrackID string, active store.Playlist, others []store.Playlist) trackRowData {
	return trackRowData{
		Track:          t,
		CurrentTrackID: currentTrackID,
		ActivePlaylist: active,
		OtherPlaylists: others,
	}
}

var uiFuncMap = template.FuncMap{
	"plural": func(n int, singular, plural string) string {
		if n == 1 {
			return singular
		}
		return plural
	},
	"makeTrackRow": makeTrackRow,
}

const uiTemplateSource = `
{{define "drawer-inner"}}
<div id="rail">
  <div class="rail-head">
    <h3>Playlists</h3>
    <div style="display:flex; gap:6px; align-items:center;">
      <button type="button" class="spotify-rail-btn" id="btn-rail-spotify" title="Import from Spotify" data-playlist-id="{{.ActivePlaylist.ID}}" onclick="RadioTransport.openCopyFind(this.dataset.playlistId)">
        <svg viewBox="0 0 24 24" fill="#1DB954" style="width:15px;height:15px;flex-shrink:0;"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.66 0 12 0zm5.521 17.34c-.24.359-.66.48-1.021.24-2.82-1.74-6.36-2.101-10.561-1.141-.418.122-.779-.179-.899-.539-.12-.421.18-.78.54-.9 4.56-1.021 8.52-.6 11.64 1.32.42.18.479.659.301 1.02zm1.44-3.3c-.301.42-.841.6-1.262.3-3.239-1.98-8.159-2.58-11.939-1.38-.479.12-1.02-.12-1.14-.6-.12-.48.12-1.021.6-1.141C9.6 9.9 15 10.561 18.72 12.84c.361.181.54.78.241 1.2zm.12-3.36C15.24 8.4 8.82 8.16 5.16 9.301c-.6.179-1.2-.181-1.38-.721-.18-.601.18-1.2.72-1.381 4.26-1.26 11.28-1.02 15.721 1.621.539.3.719 1.02.419 1.56-.299.421-1.02.599-1.559.3z"/></svg>
        <span>Import Spotify</span>
      </button>
      <button type="button" class="icon-btn" id="btn-import" title="Import station.json backup" onclick="document.getElementById('import-file').click()">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 3v12m0 0-4-4m4 4 4-4M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/></svg>
      </button>
      <a class="icon-btn" id="btn-export" href="/api/export/station.json" download="station.json" title="Export station.json backup" style="text-decoration:none;">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 21V9m0 0 4 4m-4-4-4 4M4 7V5a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v2"/></svg>
      </a>
      <a class="icon-btn" id="btn-export-urls" href="/api/export/playlist.m3u" download="{{.ActivePlaylist.Name}}.m3u" title="Export active playlist as M3U" style="text-decoration:none;">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>
      </a>
      <button type="button" class="icon-btn" id="btn-new-playlist" title="New playlist"
              hx-post="/ui/playlists" hx-prompt="Playlist name:" hx-target="#drawer-inner" hx-swap="innerHTML">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 5v14M5 12h14"/></svg>
      </button>
    </div>
  </div>
  <div id="playlist-rail-list">
    {{range .State.Playlists}}
    <div class="plist-item{{if eq .ID $.ActivePlaylist.ID}} active{{end}}"
         hx-get="/ui/playlists/{{.ID}}" hx-target="#drawer-inner" hx-swap="innerHTML">
      {{if .Image}}<img class="plist-thumb" src="{{.Image}}" alt="" loading="lazy">{{end}}
      <span class="plist-name">{{.Name}}</span>
      <span class="plist-count" id="plist-count-{{.ID}}">{{len .TrackIDs}}</span>
      <span class="plist-rename" title="Rename"
            onclick="event.stopPropagation()"
            hx-post="/ui/playlists/{{.ID}}/rename" hx-prompt="Rename playlist:" hx-target="#drawer-inner" hx-swap="innerHTML">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 20h9M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z"/></svg>
      </span>
      {{if gt (len $.State.Playlists) 1}}
      <span class="plist-del" title="Delete"
            onclick="event.stopPropagation()"
            hx-delete="/ui/playlists/{{.ID}}" hx-confirm="Delete playlist '{{.Name}}'?" hx-target="#drawer-inner" hx-swap="innerHTML">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0-1 14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2L4 6"/></svg>
      </span>
      {{end}}
    </div>
    {{end}}
  </div>
  <form id="import-form" hx-post="/ui/import" hx-encoding="multipart/form-data" hx-target="#drawer-inner" hx-swap="innerHTML" style="display:none">
    <input type="file" id="import-file" name="file" accept="application/json" onchange="if(this.files.length && confirm('Replace your current playlists with the contents of this file?')) htmx.trigger(this.form, 'submit'); else this.value='';">
  </form>
</div>

<div id="trackpanel" data-active-playlist-id="{{.ActivePlaylist.ID}}" data-active-playlist-name="{{.ActivePlaylist.Name}}"
     data-other-playlists="{{range $i, $p := .OtherPlaylists}}{{if $i}}&#10;{{end}}{{$p.ID}}|{{$p.Name}}{{end}}">
  <div id="panel-head">
    <div id="panel-title-row">
      <div id="panel-title-group">
        {{if .ActivePlaylist.Image}}<img id="panel-playlist-thumb" src="{{.ActivePlaylist.Image}}" alt="">{{end}}
        <div>
          <div id="panel-title" class="font-head">{{.ActivePlaylist.Name}}</div>
          <div id="panel-meta">{{len .Tracks}} {{plural (len .Tracks) "track" "tracks"}}</div>
        </div>
      </div>
      {{template "pill-row" .State}}
    </div>
    <form id="addform" hx-post="/ui/tracks" hx-target="#drawer-inner" hx-swap="innerHTML">
      <input type="text" id="add-input" name="input" placeholder="Paste a YouTube or Spotify URL and press Enter…" autocomplete="off">
      <button type="submit" id="add-btn">Add</button>
      <button type="button" class="icon-btn" id="btn-ai-suggest" title="AI Suggest"
              data-playlist-id="{{.ActivePlaylist.ID}}"
              onclick="RadioTransport.openAISuggest(this.dataset.playlistId)"
              style="width:auto; padding:0 12px; gap:5px; display:flex; align-items:center; font-size:12.5px; font-weight:600;">✨ AI Suggest</button>
      <button type="button" class="spotify-cta-btn" id="btn-copy-find" title="Import Spotify Playlist, Album, or Track"
              data-playlist-id="{{.ActivePlaylist.ID}}"
              onclick="RadioTransport.openCopyFind(this.dataset.playlistId)">
        <svg viewBox="0 0 24 24" fill="#1DB954" style="width:16px;height:16px;flex-shrink:0;"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.66 0 12 0zm5.521 17.34c-.24.359-.66.48-1.021.24-2.82-1.74-6.36-2.101-10.561-1.141-.418.122-.779-.179-.899-.539-.12-.421.18-.78.54-.9 4.56-1.021 8.52-.6 11.64 1.32.42.18.479.659.301 1.02zm1.44-3.3c-.301.42-.841.6-1.262.3-3.239-1.98-8.159-2.58-11.939-1.38-.479.12-1.02-.12-1.14-.6-.12-.48.12-1.021.6-1.141C9.6 9.9 15 10.561 18.72 12.84c.361.181.54.78.241 1.2zm.12-3.36C15.24 8.4 8.82 8.16 5.16 9.301c-.6.179-1.2-.181-1.38-.721-.18-.601.18-1.2.72-1.381 4.26-1.26 11.28-1.02 15.721 1.621.539.3.719 1.02.419 1.56-.299.421-1.02.599-1.559.3z"/></svg>
        Import Spotify
      </button>
    </form>
    {{if gt (len .Tracks) 4}}
    <div id="track-filter-row">
      <input type="search" id="track-filter" placeholder="Filter {{len .Tracks}} tracks…" autocomplete="off" oninput="RadioTransport.filterTracks(this.value)">
      <button type="button" class="jump-playing-btn" onclick="RadioTransport.scrollToPlaying()" title="Scroll to currently playing track">Playing</button>
    </div>
    {{end}}
  </div>
  <div id="tracklist">
    {{if not .Tracks}}
    <div id="empty-state">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>
      <p>No tracks yet. Paste a YouTube link above or tap <b>Import Spotify</b> to populate your playlist.</p>
      <button type="button" class="spotify-cta-btn" data-playlist-id="{{.ActivePlaylist.ID}}" onclick="RadioTransport.openCopyFind(this.dataset.playlistId)" style="margin-top:8px;">
        <svg viewBox="0 0 24 24" fill="#1DB954" style="width:16px;height:16px;flex-shrink:0;"><path d="M12 0C5.4 0 0 5.4 0 12s5.4 12 12 12 12-5.4 12-12S18.66 0 12 0zm5.521 17.34c-.24.359-.66.48-1.021.24-2.82-1.74-6.36-2.101-10.561-1.141-.418.122-.779-.179-.899-.539-.12-.421.18-.78.54-.9 4.56-1.021 8.52-.6 11.64 1.32.42.18.479.659.301 1.02zm1.44-3.3c-.301.42-.841.6-1.262.3-3.239-1.98-8.159-2.58-11.939-1.38-.479.12-1.02-.12-1.14-.6-.12-.48.12-1.021.6-1.141C9.6 9.9 15 10.561 18.72 12.84c.361.181.54.78.241 1.2zm.12-3.36C15.24 8.4 8.82 8.16 5.16 9.301c-.6.179-1.2-.181-1.38-.721-.18-.601.18-1.2.72-1.381 4.26-1.26 11.28-1.02 15.721 1.621.539.3.719 1.02.419 1.56-.299.421-1.02.599-1.559.3z"/></svg>
        Import Spotify Playlist
      </button>
    </div>
    {{else}}
      {{range .Tracks}}
        {{template "track-row" (makeTrackRow . $.State.CurrentTrackID $.ActivePlaylist $.OtherPlaylists)}}
      {{end}}
    {{end}}
  </div>
  <button type="button" id="drawer-close-bar" onclick="RadioTransport.closeDrawer()">
    <span>Done · Back to Player</span>
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"><path d="M18 15l-6-6-6 6"/></svg>
  </button>
</div>
{{end}}

{{define "pill-row"}}
<div class="pill-row" id="pill-row">
  <div class="pill{{if .Shuffle}} active{{end}}" id="pill-shuffle"
       hx-post="/ui/flags/shuffle" hx-target="#pill-row" hx-swap="outerHTML"
       title="Weighted shuffle: favorites &amp; more-played tracks come up more often">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M16 3h5v5M4 20 21 3M21 16v5h-5M15 15l6 6M4 4l5 5"/></svg>
    Shuffle
  </div>
  <div class="pill{{if .Repeat}} active{{end}}" id="pill-repeat"
       hx-post="/ui/flags/repeat" hx-target="#pill-row" hx-swap="outerHTML"
       title="Repeat playlist">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M17 1l4 4-4 4M3 11V9a4 4 0 0 1 4-4h14M7 23l-4-4 4-4M21 13v2a4 4 0 0 1-4 4H3"/></svg>
    Repeat
  </div>
  <div class="pill autopilot{{if .Autopilot}} active{{end}}" id="pill-autopilot"
       hx-post="/ui/flags/autopilot" hx-target="#pill-row" hx-swap="outerHTML"
       title="Autopilot: when the playlist runs dry, AI keeps the radio going with more tracks like what you've been playing">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 2v4M12 18v4M4.93 4.93l2.83 2.83M16.24 16.24l2.83 2.83M2 12h4M18 12h4M4.93 19.07l2.83-2.83M16.24 7.76l2.83-2.83"/></svg>
    Autopilot
  </div>
</div>
{{end}}

{{define "track-row"}}
<div class="track{{if eq .Track.ID .CurrentTrackID}} playing{{end}}"
     id="track-{{.Track.ID}}"
     data-track-id="{{.Track.ID}}"
     data-video-id="{{.Track.VideoID}}"
     data-title="{{.Track.Title}}"
     data-thumb="{{.Track.Thumb}}"
     data-playlist-name="{{.ActivePlaylist.Name}}"
     onclick="RadioTransport.onTrackRowClick(event, this)">
  <div class="track-thumb" style="background-image:url('{{.Track.Thumb}}')">
    <div class="playmark"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg></div>
  </div>
  <div class="track-info">
    <div class="track-title">{{.Track.Title}}</div>
    <div class="track-sub"><span class="track-plays" id="plays-{{.Track.ID}}">▶ {{.Track.PlayCount}} plays</span></div>
  </div>
  <div class="track-actions" onclick="event.stopPropagation()">
    <button type="button" class="fav{{if .Track.Favorite}} active{{end}}" title="Favorite"
            hx-post="/ui/tracks/{{.Track.ID}}/fav" hx-target="#track-{{.Track.ID}}" hx-swap="outerHTML">
      <svg viewBox="0 0 24 24" fill="{{if .Track.Favorite}}currentColor{{else}}none{{end}}" stroke="currentColor" stroke-width="2"><path d="M12 2l3.09 6.26L22 9.27l-5 4.87L18.18 21 12 17.77 5.82 21 7 14.14l-5-4.87 6.91-1.01L12 2z"/></svg>
    </button>
    <button type="button" class="move" title="Move to playlist"
            onclick="RadioTransport.toggleMoveMenu(event, this)">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4M10 17l5-5-5-5M15 12H3"/></svg>
    </button>
    <button type="button" class="ai" title="Find similar with AI"
            data-playlist-id="{{.ActivePlaylist.ID}}" data-video-id="{{.Track.VideoID}}"
            onclick="RadioTransport.openAISimilar(this.dataset.playlistId, this.dataset.videoId)">
      <svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 2l1.7 5.3L19 9l-5.3 1.7L12 16l-1.7-5.3L5 9l5.3-1.7L12 2z"/><path d="M19 13.5l.9 2.6L22.5 17l-2.6.9-.9 2.6-.9-2.6-2.6-.9 2.6-.9 .9-2.6z"/></svg>
    </button>
    <button type="button" class="remove" title="Remove from playlist"
            hx-delete="/ui/playlists/{{.ActivePlaylist.ID}}/tracks/{{.Track.ID}}" hx-target="#track-{{.Track.ID}}" hx-swap="outerHTML">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0-1 14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2L4 6"/></svg>
    </button>
  </div>
</div>
{{end}}

{{define "nowplaying-oob"}}
<div id="nowplaying" hx-swap-oob="true">
  {{if .CurrentTrack}}
  <div id="np-title">{{.CurrentTrack.Title}}</div>
  <div id="np-sub">{{.ActivePlaylist.Name}}</div>
  {{else}}
  <div id="np-title" class="empty">Nothing queued</div>
  <div id="np-sub">Open the drawer and add a track to get started</div>
  {{end}}
</div>
<div id="below-orb" hx-swap-oob="true" {{if .ShowVideo}}style="display:none"{{end}}>
  {{if .CurrentTrack}}
  <div class="big-title" id="big-title">{{.CurrentTrack.Title}}</div>
  <div class="big-sub" id="big-sub">from “{{.ActivePlaylist.Name}}” · {{.CurrentTrack.PlayCount}} plays</div>
  {{else}}
  <div class="big-title" id="big-title">Nothing playing</div>
  <div class="big-sub" id="big-sub">Add a track from the Playlists drawer above</div>
  {{end}}
  <div id="stage-actions">
    <button type="button" class="stage-chip{{if and .CurrentTrack .CurrentTrack.Favorite}} fav-active{{end}}" id="stage-fav-btn"
            onclick="RadioTransport.toggleCurrentFav()" {{if not .CurrentTrack}}style="display:none"{{end}}>
      <svg viewBox="0 0 24 24" fill="{{if and .CurrentTrack .CurrentTrack.Favorite}}currentColor{{else}}none{{end}}" stroke="currentColor" stroke-width="2"><path d="M12 2l3.09 6.26L22 9.27l-5 4.87L18.18 21 12 17.77 5.82 21 7 14.14l-5-4.87 6.91-1.01L12 2z"/></svg>
      <span>Favorite</span>
    </button>
    <button type="button" class="stage-chip" id="stage-sleep-pill" onclick="RadioTransport.openSleepModal()">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>
      <span id="stage-sleep-label">Sleep</span>
    </button>
    <button type="button" class="stage-chip next-chip" id="stage-next-chip" onclick="RadioTransport.stepNext(true)" {{if not .NextTrack}}style="display:none"{{end}}>
      <span class="next-prefix">Next:</span>
      <span id="stage-next-title">{{if .NextTrack}}{{.NextTrack.Title}}{{end}}</span>
      <svg viewBox="0 0 24 24" fill="currentColor" style="width:12px;height:12px;flex-shrink:0;"><path d="M16 6h2v12h-2zM6 6l8.5 6L6 18z"/></svg>
    </button>
  </div>
</div>
<div id="orb-inner" hx-swap-oob="true">
  {{if and .CurrentTrack .CurrentTrack.Thumb}}
  <div class="orb-empty" id="orb-empty" style="display:none">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>
  </div>
  <img id="orb-img" src="{{.CurrentTrack.Thumb}}" style="display:block">
  {{else}}
  <div class="orb-empty" id="orb-empty" style="display:flex">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>
  </div>
  <img id="orb-img" style="display:none">
  {{end}}
</div>
{{end}}
`

var uiTemplates = template.Must(template.New("ui").Funcs(uiFuncMap).Parse(uiTemplateSource))

func (s *Server) buildUIViewData(r *http.Request, st store.StationState) uiViewData {
	u := auth.UserFromContext(r.Context())
	pl := st.ActivePlaylist()
	var activePl store.Playlist
	if pl != nil {
		activePl = *pl
	}
	var others []store.Playlist
	for _, p := range st.Playlists {
		if p.ID != activePl.ID {
			others = append(others, p)
		}
	}
	tracks := st.ActiveTracks()
	cur := st.CurrentTrack()

	initTrackID := st.CurrentTrackID
	initVideoID := ""
	if cur != nil {
		initVideoID = cur.VideoID
	} else if len(tracks) > 0 {
		t := tracks[0]
		cur = &t
		initTrackID = tracks[0].ID
		initVideoID = tracks[0].VideoID
	}
	var nextTrack *store.Track
	if nextID, _ := st.PickNextTrackID(false); nextID != "" {
		if nt, ok := st.Tracks[nextID]; ok {
			ntc := nt
			nextTrack = &ntc
		}
	}
	showVid := st.ShowVideo != nil && *st.ShowVideo

	return uiViewData{
		User:           u,
		IsRemote:       !auth.IsLocalDirect(r),
		State:          st,
		ActivePlaylist: activePl,
		OtherPlaylists: others,
		Tracks:         tracks,
		CurrentTrack:   cur,
		NextTrack:      nextTrack,
		InitialTrackID: initTrackID,
		InitialVideoID: initVideoID,
		ShowVideo:      showVid,
	}
}

func (s *Server) buildTrackTriggerPayload(t store.Track, playlistName string, next *store.Track) map[string]any {
	payload := map[string]any{
		"trackId":      t.ID,
		"videoId":      t.VideoID,
		"title":        t.Title,
		"thumb":        t.Thumb,
		"playlistName": playlistName,
		"favorite":     t.Favorite,
	}
	if next != nil {
		payload["prefetchVideoId"] = next.VideoID
		payload["nextTrackId"] = next.ID
		payload["nextVideoId"] = next.VideoID
		payload["nextTitle"] = next.Title
		payload["nextThumb"] = next.Thumb
		payload["nextPlaylistName"] = playlistName
	}
	return payload
}

func (s *Server) buildNextTriggerPayload(playlistName string, next *store.Track) map[string]string {
	if next == nil {
		return nil
	}
	return map[string]string{
		"videoId":          next.VideoID,
		"nextTrackId":      next.ID,
		"nextVideoId":      next.VideoID,
		"nextTitle":        next.Title,
		"nextThumb":        next.Thumb,
		"nextPlaylistName": playlistName,
	}
}

func (s *Server) renderDrawerInner(w http.ResponseWriter, r *http.Request, st store.StationState) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := s.buildUIViewData(r, st)
	_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
}

func (s *Server) setHXTrigger(w http.ResponseWriter, event string, payload any) {
	b, err := json.Marshal(map[string]any{event: payload})
	if err == nil {
		w.Header().Set("HX-Trigger", string(b))
	}
}

func (s *Server) handleUIDrawer(w http.ResponseWriter, r *http.Request) {
	fs := s.fileStoreForRequest(r)
	_, st, err := fs.ReadState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("sync") == "1" {
		data := s.buildUIViewData(r, st)
		if data.CurrentTrack != nil {
			s.setHXTrigger(w, "trackActivated", s.buildTrackTriggerPayload(*data.CurrentTrack, data.ActivePlaylist.Name, data.NextTrack))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
		_ = uiTemplates.ExecuteTemplate(w, "nowplaying-oob", data)
		return
	}
	s.renderDrawerInner(w, r, st)
}

func (s *Server) handleSelectPlaylist(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	fs := s.fileStoreForRequest(r)
	var switchedTrack *store.Track
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		pl := st.PlaylistByID(pid)
		if pl == nil {
			return store.ErrNotFound
		}
		st.ActivePlaylistID = pid
		inActive := false
		for _, tid := range pl.TrackIDs {
			if tid == st.CurrentTrackID {
				inActive = true
				break
			}
		}
		if !inActive && len(pl.TrackIDs) > 0 {
			st.CurrentTrackID = pl.TrackIDs[0]
			if t, ok := st.Tracks[st.CurrentTrackID]; ok {
				tc := t
				switchedTrack = &tc
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := s.buildUIViewData(r, st)
	if s.Streams != nil {
		var ids []string
		if data.NextTrack != nil && data.NextTrack.VideoID != "" {
			ids = append(ids, data.NextTrack.VideoID)
		}
		if pl := st.ActivePlaylist(); pl != nil {
			for _, tid := range pl.TrackIDs {
				if t, ok := st.Tracks[tid]; ok {
					ids = append(ids, t.VideoID)
					if len(ids) >= 20 {
						break
					}
				}
			}
		}
		if len(ids) > 0 {
			s.Streams.EnqueueWarm(ids...)
		}
	}
	if switchedTrack != nil {
		s.setHXTrigger(w, "trackActivated", s.buildTrackTriggerPayload(*switchedTrack, data.ActivePlaylist.Name, data.NextTrack))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
		_ = uiTemplates.ExecuteTemplate(w, "nowplaying-oob", data)
		return
	}
	if data.NextTrack != nil {
		s.setHXTrigger(w, "prefetchStream", s.buildNextTriggerPayload(data.ActivePlaylist.Name, data.NextTrack))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
}

func (s *Server) handleCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	if name == "" {
		_ = r.ParseForm()
		name = strings.TrimSpace(r.FormValue("name"))
	}
	fs := s.fileStoreForRequest(r)
	if name == "" {
		_, st, _ := fs.ReadState()
		s.renderDrawerInner(w, r, st)
		return
	}
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		pl := store.Playlist{
			ID:       store.GenerateUID(),
			Name:     name,
			TrackIDs: []string{},
		}
		st.Playlists = append(st.Playlists, pl)
		st.ActivePlaylistID = pl.ID
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renderDrawerInner(w, r, st)
}

func (s *Server) handleRenamePlaylist(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	name := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	if name == "" {
		_ = r.ParseForm()
		name = strings.TrimSpace(r.FormValue("name"))
	}
	fs := s.fileStoreForRequest(r)
	if name == "" {
		_, st, _ := fs.ReadState()
		s.renderDrawerInner(w, r, st)
		return
	}
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		pl := st.PlaylistByID(pid)
		if pl == nil {
			return store.ErrNotFound
		}
		pl.Name = name
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renderDrawerInner(w, r, st)
}

func (s *Server) handleDeletePlaylist(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	fs := s.fileStoreForRequest(r)
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		if len(st.Playlists) <= 1 {
			return nil
		}
		kept := make([]store.Playlist, 0, len(st.Playlists)-1)
		for _, p := range st.Playlists {
			if p.ID != pid {
				kept = append(kept, p)
			}
		}
		if len(kept) > 0 {
			st.Playlists = kept
			if st.ActivePlaylistID == pid {
				st.ActivePlaylistID = kept[0].ID
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renderDrawerInner(w, r, st)
}

func (s *Server) handleToggleFlag(w http.ResponseWriter, r *http.Request) {
	flag := r.PathValue("flag")
	fs := s.fileStoreForRequest(r)
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		switch flag {
		case "shuffle":
			st.Shuffle = !st.Shuffle
		case "repeat":
			st.Repeat = !st.Repeat
		case "autopilot":
			st.Autopilot = !st.Autopilot
		case "showVideo":
			cur := st.ShowVideo == nil || *st.ShowVideo
			next := !cur
			st.ShowVideo = &next
		case "videoMaximized":
			st.VideoMaximized = !st.VideoMaximized
		case "volume":
			_ = r.ParseForm()
			if v, err := strconv.Atoi(r.FormValue("volume")); err == nil && v >= 0 && v <= 100 {
				st.Volume = v
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if flag == "showVideo" || flag == "videoMaximized" || flag == "volume" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data := s.buildUIViewData(r, st)
	if data.NextTrack != nil {
		if s.Streams != nil && data.NextTrack.VideoID != "" {
			s.Streams.EnqueueWarm(data.NextTrack.VideoID)
		}
		s.setHXTrigger(w, "prefetchStream", s.buildNextTriggerPayload(data.ActivePlaylist.Name, data.NextTrack))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = uiTemplates.ExecuteTemplate(w, "pill-row", st)
}

func (s *Server) handlePlayTrack(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("id")
	fs := s.fileStoreForRequest(r)
	var played store.Track
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		t, ok := st.Tracks[tid]
		if !ok {
			return store.ErrNotFound
		}
		t.PlayCount++
		st.Tracks[tid] = t
		st.CurrentTrackID = tid
		played = t
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := s.buildUIViewData(r, st)
	if s.Streams != nil && data.NextTrack != nil && data.NextTrack.VideoID != "" {
		s.Streams.EnqueueWarm(data.NextTrack.VideoID)
	}

	s.setHXTrigger(w, "trackActivated", s.buildTrackTriggerPayload(played, data.ActivePlaylist.Name, data.NextTrack))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = uiTemplates.ExecuteTemplate(w, "nowplaying-oob", data)
	fmt.Fprintf(w, `<span class="track-plays" id="plays-%s" hx-swap-oob="true">▶ %d plays</span>`,
		template.HTMLEscapeString(played.ID), played.PlayCount)
}

func (s *Server) handlePlayerStep(prev bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fs := s.fileStoreForRequest(r)
		_, stSnapshot, err := fs.ReadState()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		alreadyPlayingID := strings.TrimSpace(r.URL.Query().Get("alreadyPlaying"))
		var nextID string
		var needAutopilot bool
		if !prev && alreadyPlayingID != "" {
			if _, ok := stSnapshot.Tracks[alreadyPlayingID]; ok {
				nextID = alreadyPlayingID
			}
		}
		if nextID == "" {
			nextID, needAutopilot = stSnapshot.PickNextTrackID(prev)
		}

		autopilotAdded := false
		if nextID == "" && needAutopilot && s.DJ != nil {
			u := auth.UserFromContext(r.Context())
			if s.Auth == nil || s.Auth.AllowAISuggest(u) {
				seed := buildAutopilotSeed(stSnapshot)
				ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
				suggestions, djErr := s.DJ.GatherSuggestions(ctx, seed, 8)
				cancel()
				if djErr == nil && len(suggestions) > 0 {
					inputs := make([]store.NewTrackInput, 0, len(suggestions))
					for _, sg := range suggestions {
						title := sg.Title
						if sg.Artist != "" {
							title = sg.Title + " — " + sg.Artist
						}
						inputs = append(inputs, store.NewTrackInput{
							VideoID:   sg.VideoID,
							Title:     title,
							Thumb:     sg.Thumb,
							Autopilot: true,
						})
					}
					_, _, _ = fs.Mutate(func(st *store.StationState) error {
						added, first := store.AddTracksToPlaylist(st, st.ActivePlaylistID, inputs)
						if added > 0 && first != nil {
							nextID = first.ID
							autopilotAdded = true
						}
						return nil
					})
				}
			}
		}
		if nextID == "" {
			if pl := stSnapshot.ActivePlaylist(); pl != nil && len(pl.TrackIDs) > 0 {
				nextID = pl.TrackIDs[0]
			}
		}

		if nextID == "" {
			s.setHXTrigger(w, "stopPlayback", map[string]bool{"stopped": true})
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var played store.Track
		_, st, err := fs.Mutate(func(st *store.StationState) error {
			t, ok := st.Tracks[nextID]
			if !ok {
				return store.ErrNotFound
			}
			t.PlayCount++
			st.Tracks[nextID] = t
			st.CurrentTrackID = nextID
			played = t
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		data := s.buildUIViewData(r, st)
		if s.Streams != nil && data.NextTrack != nil && data.NextTrack.VideoID != "" {
			s.Streams.EnqueueWarm(data.NextTrack.VideoID)
		}

		triggerEvent := "loadTrack"
		if alreadyPlayingID != "" && played.ID == alreadyPlayingID {
			triggerEvent = "trackActivated"
		}
		s.setHXTrigger(w, triggerEvent, s.buildTrackTriggerPayload(played, data.ActivePlaylist.Name, data.NextTrack))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = uiTemplates.ExecuteTemplate(w, "nowplaying-oob", data)
		if autopilotAdded {
			fmt.Fprint(w, `<div id="drawer-inner" hx-swap-oob="innerHTML">`)
			_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
			fmt.Fprint(w, `</div>`)
		} else {
			fmt.Fprintf(w, `<span class="track-plays" id="plays-%s" hx-swap-oob="true">▶ %d plays</span>`,
				template.HTMLEscapeString(played.ID), played.PlayCount)
		}
	}
}

func buildAutopilotSeed(st store.StationState) string {
	tracks := st.ActiveTracks()
	var pool []string
	seen := make(map[string]bool)
	add := func(title string) {
		title = strings.TrimSpace(title)
		if title != "" && !seen[title] && len(pool) < 6 {
			seen[title] = true
			pool = append(pool, title)
		}
	}
	if cur := st.CurrentTrack(); cur != nil {
		add(cur.Title)
	}
	for _, t := range tracks {
		if t.Favorite {
			add(t.Title)
		}
	}
	sorted := append([]store.Track(nil), tracks...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].PlayCount > sorted[j].PlayCount
	})
	for _, t := range sorted {
		add(t.Title)
	}
	if len(pool) == 0 {
		return "eclectic, high-quality music mix"
	}
	return "Music in the style of / similar to: " + strings.Join(pool, "; ")
}

func (s *Server) handleToggleFav(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("id")
	fs := s.fileStoreForRequest(r)
	var updated store.Track
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		t, ok := st.Tracks[tid]
		if !ok {
			return store.ErrNotFound
		}
		t.Favorite = !t.Favorite
		st.Tracks[tid] = t
		updated = t
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.setHXTrigger(w, "favUpdated", map[string]any{
		"trackId":  updated.ID,
		"favorite": updated.Favorite,
	})
	data := s.buildUIViewData(r, st)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = uiTemplates.ExecuteTemplate(w, "track-row", makeTrackRow(updated, st.CurrentTrackID, data.ActivePlaylist, data.OtherPlaylists))
}

func (s *Server) handleRemoveTrack(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	tid := r.PathValue("tid")
	fs := s.fileStoreForRequest(r)
	var count int
	_, _, err := fs.Mutate(func(st *store.StationState) error {
		pl := st.PlaylistByID(pid)
		if pl == nil {
			return store.ErrNotFound
		}
		kept := make([]string, 0, len(pl.TrackIDs))
		for _, id := range pl.TrackIDs {
			if id != tid {
				kept = append(kept, id)
			}
		}
		pl.TrackIDs = kept
		count = len(kept)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	word := "tracks"
	if count == 1 {
		word = "track"
	}
	fmt.Fprintf(w, `<div id="panel-meta" hx-swap-oob="true">%d %s</div>`, count, word)
	fmt.Fprintf(w, `<span class="plist-count" id="plist-count-%s" hx-swap-oob="true">%d</span>`,
		template.HTMLEscapeString(pid), count)
	if count == 0 {
		fmt.Fprint(w, `<div id="tracklist" hx-swap-oob="innerHTML"><div id="empty-state"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg><p>No tracks yet. Paste a YouTube link above to add your first one.</p></div></div>`)
	}
}

func (s *Server) handleMoveTrack(w http.ResponseWriter, r *http.Request) {
	fromID := r.PathValue("pid")
	tid := r.PathValue("tid")
	toID := strings.TrimSpace(r.URL.Query().Get("to"))
	fs := s.fileStoreForRequest(r)
	var fromCount, toCount int
	_, _, err := fs.Mutate(func(st *store.StationState) error {
		fromPl := st.PlaylistByID(fromID)
		toPl := st.PlaylistByID(toID)
		if fromPl == nil || toPl == nil {
			return store.ErrNotFound
		}
		kept := make([]string, 0, len(fromPl.TrackIDs))
		for _, id := range fromPl.TrackIDs {
			if id != tid {
				kept = append(kept, id)
			}
		}
		fromPl.TrackIDs = kept
		already := false
		for _, id := range toPl.TrackIDs {
			if id == tid {
				already = true
				break
			}
		}
		if !already {
			toPl.TrackIDs = append(toPl.TrackIDs, tid)
		}
		fromCount = len(fromPl.TrackIDs)
		toCount = len(toPl.TrackIDs)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	word := "tracks"
	if fromCount == 1 {
		word = "track"
	}
	fmt.Fprintf(w, `<div id="panel-meta" hx-swap-oob="true">%d %s</div>`, fromCount, word)
	fmt.Fprintf(w, `<span class="plist-count" id="plist-count-%s" hx-swap-oob="true">%d</span>`,
		template.HTMLEscapeString(fromID), fromCount)
	fmt.Fprintf(w, `<span class="plist-count" id="plist-count-%s" hx-swap-oob="true">%d</span>`,
		template.HTMLEscapeString(toID), toCount)
}

var (
	reExactVideoID = regexp.MustCompile(`^[\w-]{11}$`)
	reEmbedVideoID = regexp.MustCompile(`/(?:embed|shorts|v)/([\w-]{11})`)
	reAnyVideoID   = regexp.MustCompile(`[\w-]{11}`)
)

func parseYouTubeVideoID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if reExactVideoID.MatchString(raw) {
		return raw
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host := strings.ToLower(u.Host)
		if !strings.Contains(host, "youtube.com") && !strings.Contains(host, "youtu.be") && !strings.Contains(host, "youtube-nocookie.com") {
			return ""
		}
		if strings.Contains(host, "youtu.be") {
			id := strings.Trim(u.Path, "/")
			if reExactVideoID.MatchString(id) {
				return id
			}
		}
		if v := u.Query().Get("v"); reExactVideoID.MatchString(v) {
			return v
		}
		if m := reEmbedVideoID.FindStringSubmatch(u.Path); len(m) == 2 {
			return m[1]
		}
		return ""
	}
	return reAnyVideoID.FindString(raw)
}

func fetchYouTubeOEmbed(ctx context.Context, videoID string) (title string, thumb string) {
	title = videoID
	thumb = "https://i.ytimg.com/vi/" + videoID + "/hqdefault.jpg"

	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	oembedURL := "https://www.youtube.com/oembed?url=" + url.QueryEscape("https://www.youtube.com/watch?v="+videoID) + "&format=json"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, oembedURL, nil)
	if err != nil {
		return title, thumb
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return title, thumb
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return title, thumb
	}
	var meta struct {
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&meta); err == nil {
		if strings.TrimSpace(meta.Title) != "" {
			title = strings.TrimSpace(meta.Title)
		}
		if strings.TrimSpace(meta.ThumbnailURL) != "" {
			thumb = strings.TrimSpace(meta.ThumbnailURL)
		}
	}
	return title, thumb
}

func (s *Server) handleAddTrack(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFromContext(r.Context())
	_ = r.ParseForm()
	raw := strings.TrimSpace(r.FormValue("input"))
	fs := s.fileStoreForRequest(r)

	// If the user pasted a Spotify link into the quick-add input, redirect to Copy & Find
	if strings.Contains(strings.ToLower(raw), "spotify.") || strings.HasPrefix(strings.ToLower(raw), "spotify:") {
		_, st, _ := fs.ReadState()
		target := "/find.html?playlist=" + url.QueryEscape(st.ActivePlaylistID) + "&paste=" + url.QueryEscape(raw)
		log.Printf("ui add-track [user=%s]: detected spotify link %q -> redirecting to %s", u.Email, raw, target)
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	vid := parseYouTubeVideoID(raw)
	if vid == "" {
		log.Printf("ui add-track [user=%s]: unparseable input %q", u.Email, raw)
		_, st, _ := fs.ReadState()
		s.renderDrawerInner(w, r, st)
		return
	}
	log.Printf("ui add-track [user=%s]: raw=%q videoId=%s", u.Email, raw, vid)

	_, snapshot, _ := fs.ReadState()
	var title, thumb string
	for _, t := range snapshot.Tracks {
		if t.VideoID == vid {
			title = t.Title
			thumb = t.Thumb
			break
		}
	}
	if title == "" {
		title, thumb = fetchYouTubeOEmbed(r.Context(), vid)
	}

	var autoPlay *store.Track
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		_, tr := store.AddTracksToPlaylist(st, st.ActivePlaylistID, []store.NewTrackInput{
			{VideoID: vid, Title: title, Thumb: thumb},
		})
		if st.CurrentTrackID == "" && tr != nil {
			st.CurrentTrackID = tr.ID
			autoPlay = tr
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Streams != nil && vid != "" {
		s.Streams.EnqueueWarm(vid)
	}
	data := s.buildUIViewData(r, st)
	if autoPlay != nil {
		s.setHXTrigger(w, "loadTrack", s.buildTrackTriggerPayload(*autoPlay, data.ActivePlaylist.Name, data.NextTrack))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
		_ = uiTemplates.ExecuteTemplate(w, "nowplaying-oob", data)
		return
	}
	if data.NextTrack != nil {
		s.setHXTrigger(w, "prefetchStream", s.buildNextTriggerPayload(data.ActivePlaylist.Name, data.NextTrack))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = uiTemplates.ExecuteTemplate(w, "drawer-inner", data)
}

func (s *Server) handleExportStation(w http.ResponseWriter, r *http.Request) {
	fs := s.fileStoreForRequest(r)
	_, st, err := fs.ReadState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="station.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
}

func (s *Server) handleExportM3U(w http.ResponseWriter, r *http.Request) {
	fs := s.fileStoreForRequest(r)
	_, st, err := fs.ReadState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tracks := st.ActiveTracks()
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	var buf bytes.Buffer
	buf.WriteString("#EXTM3U\n")
	for _, t := range tracks {
		buf.WriteString(fmt.Sprintf("#EXTINF:-1,%s\nhttps://www.youtube.com/watch?v=%s\n", t.Title, t.VideoID))
	}
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleImportStation(w http.ResponseWriter, r *http.Request) {
	fs := s.fileStoreForRequest(r)
	if err := r.ParseMultipartForm(store.BodyLimit); err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, store.BodyLimit))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var imported store.StationState
	if err := json.Unmarshal(b, &imported); err != nil || len(imported.Playlists) == 0 {
		http.Error(w, "invalid station.json", http.StatusBadRequest)
		return
	}
	_, st, err := fs.Mutate(func(st *store.StationState) error {
		*st = imported
		st.CurrentTrackID = ""
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renderDrawerInner(w, r, st)
}

// renderIndexPage executes index.html as a server-side Go template with the user's station state.
func (s *Server) renderIndexPage(w http.ResponseWriter, r *http.Request, indexBytes []byte) {
	fs := s.fileStoreForRequest(r)
	_, st, err := fs.ReadState()
	if err != nil {
		st = store.DefaultStationState()
	}
	data := s.buildUIViewData(r, st)

	tmpl, err := template.New("ui").Funcs(uiFuncMap).Parse(uiTemplateSource)
	if err != nil {
		http.Error(w, "template init error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tmpl.New("index").Parse(string(indexBytes)); err != nil {
		http.Error(w, "template parse error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "index", data); err != nil {
		http.Error(w, "template exec error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "origin-when-cross-origin")
	_, _ = w.Write(buf.Bytes())
}
