/* ---------------------------------------------------------------
   Station storage — shared by index.html, ai-builder.html, find.html.

   The Go server owns data/station.json. Each page is served with the current
   state injected as window.__STATION__ = {rev, state}, so it starts with real
   data synchronously. Writes are PUT /api/state carrying the rev they were based
   on; a stale rev gets 409 and the page adopts the server's copy instead of
   overwriting another tab's changes. Other tabs hear about writes over a
   BroadcastChannel and refetch.

   State shape:
   {
     playlists: [ {id, name, trackIds: [id,...]} ],
     tracks: { id: {id, videoId, title, thumb, favorite, playCount, addedAt} },
     activePlaylistId, currentTrackId, shuffle, repeat, volume, ...
   }
--------------------------------------------------------------- */
const Station = (() => {
  const chan = new BroadcastChannel('station');
  let rev = (window.__STATION__ && window.__STATION__.rev) || 0;
  let pending = null;
  let timer = null;
  let flushing = Promise.resolve();
  let conflictHandler = () => {};
  let remoteHandler = () => {};

  function uid(){ return Math.random().toString(36).slice(2,10) + Date.now().toString(36); }

  function escapeHtml(s){
    const d = document.createElement('div');
    d.textContent = s == null ? '' : s;
    return d.innerHTML;
  }

  // addTracks deduplicates items ({videoId, title, thumb, autopilot?}) against
  // state.tracks by videoId, appends any not already in pl.trackIds, and returns
  // the number of tracks newly added to pl.
  function addTracks(state, pl, items){
    let added = 0;
    for(const item of items){
      let track = Object.values(state.tracks).find(t => t.videoId === item.videoId);
      if(!track){
        track = {
          id: uid(),
          videoId: item.videoId,
          title: item.title || item.videoId,
          thumb: item.thumb || `https://i.ytimg.com/vi/${item.videoId}/mqdefault.jpg`,
          favorite: false,
          playCount: 0,
          addedAt: Date.now()
        };
        if(item.autopilot) track.autopilot = true;
        state.tracks[track.id] = track;
      }
      if(!pl.trackIds.includes(track.id)){
        pl.trackIds.push(track.id);
        added++;
      }
    }
    return added;
  }

  function defaultState(){
    const firstId = uid();
    return {
      playlists: [{id:firstId, name:'My Station', trackIds:[]}],
      tracks: {},
      activePlaylistId: firstId,
      currentTrackId: null,
      shuffle: false,
      repeat: true,
      volume: 70,
      showVideo: true,
      videoMaximized: false,
      autopilot: false
    };
  }

  function initial(){
    const s = window.__STATION__ && window.__STATION__.state;
    return s ? s : defaultState();
  }

  async function fetchState(){
    const res = await fetch('/api/state', {cache: 'no-store'});
    const data = await res.json();
    rev = data.rev;
    return data.state || defaultState();
  }

  async function put(state){
    const res = await fetch('/api/state', {
      method: 'PUT',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({rev, state})
    });
    if(res.status === 409) return {conflict: true};
    if(!res.ok){
      const data = await res.json().catch(() => ({}));
      throw new Error(data.error || `save failed: HTTP ${res.status}`);
    }
    rev = (await res.json()).rev;
    chan.postMessage({rev});
    return {conflict: false};
  }

  function flush(){
    clearTimeout(timer);
    timer = null;
    if(!pending) return flushing;
    const state = pending;
    pending = null;
    flushing = flushing.then(async () => {
      try{
        const r = await put(state);
        if(r.conflict) conflictHandler(await fetchState());
      }catch(e){
        console.error('station save failed', e);
      }
    });
    return flushing;
  }

  // save queues a debounced write of the whole state (the player page's model).
  function save(state){
    pending = state;
    clearTimeout(timer);
    timer = setTimeout(flush, 300);
  }

  // update applies fn to the freshest server state and writes it, retrying on conflict
  // (the model for pages that make discrete changes: AI builder, Copy & Find).
  async function update(fn){
    await flush();
    for(let attempt = 0; attempt < 4; attempt++){
      const state = await fetchState();
      const result = fn(state);
      const r = await put(state);
      if(!r.conflict) return {state, result};
    }
    throw new Error('could not save: the station kept changing underneath');
  }

  chan.onmessage = async (e) => {
    if(!e.data || e.data.rev <= rev) return;
    if(pending) await flush(); // our unsaved change goes first; a conflict adopts theirs
    remoteHandler(await fetchState());
  };

  // last-chance save when the tab closes (keepalive bodies are capped at 64 KB)
  window.addEventListener('pagehide', () => {
    if(!pending) return;
    const body = JSON.stringify({rev, state: pending});
    if(body.length < 60000){
      fetch('/api/state', {method:'PUT', headers:{'Content-Type':'application/json'}, body, keepalive:true});
    }
  });

  return {
    uid, escapeHtml, addTracks, defaultState, initial, fetchState, save, flush, update,
    onConflict(fn){ conflictHandler = fn; },
    onRemoteChange(fn){ remoteHandler = fn; }
  };
})();
