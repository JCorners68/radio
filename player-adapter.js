/* player-adapter.js — isolates playback transport behind a uniform PlayerAdapter contract.
 *
 * Implementations:
 *   - createYouTubeAdapter: YouTube IFrame API (desktop default, supports video + audio).
 *   - createAudioStreamAdapter: HTML5 <audio> streaming /api/stream/{videoId} (iPad/iPhone Safari
 *     default; supports lock-screen background playback, Control Center MediaSession, and
 *     uninterrupted multi-track auto-advance without user re-tap).
 *   - createHybridAdapter: switches seamlessly between AudioStreamAdapter (default on iOS) and
 *     YouTubeAdapter (when the user toggles Video on mobile).
 *   - createAutoAdapter: selects HybridAdapter on iPadOS/iOS and YouTubeAdapter on desktop.
 *
 * Adapter contract:
 *   kind: 'youtube' | 'audio'
 *   init()
 *   isReady() -> boolean
 *   cue(videoId)
 *   load(videoId)
 *   play()
 *   pause()
 *   seekTo(seconds)
 *   setVolume(vol0to100)
 *   getCurrentTime() -> seconds
 *   getDuration() -> seconds
 *   prefetch(videoId)
 *   unlock()
 *   setVideoMode(enabled, currentVideoId, shouldPlay)
 */
(function(){
  function isAppleMobile(){
    const params = new URLSearchParams(window.location.search);
    if(params.get('player') === 'audio') return true;
    if(params.get('player') === 'youtube') return false;
    const ua = navigator.userAgent || '';
    if(/iPad|iPhone|iPod/i.test(ua)) return true;
    // iPadOS 13+ desktop-class Safari reports MacIntel with touch points
    if(navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1) return true;
    if(window.navigator.standalone === true) return true;
    return false;
  }

  function createYouTubeAdapter(opts){
    const elementId = (opts && opts.elementId) || 'ytplayer';
    const onReadyCb = (opts && opts.onReady) || function(){};
    const onStateChangeCb = (opts && opts.onStateChange) || function(){};

    let ytPlayer = null;
    let ready = false;
    let scriptAdded = false;
    let pendingAction = null;
    let pendingVolume = null;

    function attachPlayer(){
      if(ytPlayer) return;
      ytPlayer = new YT.Player(elementId, {
        height: '100%',
        width: '100%',
        playerVars: {
          autoplay: 0,
          controls: 1,
          disablekb: 0,
          modestbranding: 1,
          rel: 0,
          fs: 0,
          playsinline: 1,
          origin: window.location.origin,
          widget_referrer: window.location.origin
        },
        events: {
          onReady: function(){
            ready = true;
            if(pendingVolume !== null && ytPlayer.setVolume){
              ytPlayer.setVolume(pendingVolume);
            }
            if(pendingAction){
              const act = pendingAction;
              pendingAction = null;
              act();
            }
            onReadyCb();
          },
          onStateChange: function(e){
            if(e.data === YT.PlayerState.BUFFERING){
              onStateChangeCb('loading');
            }else if(e.data === YT.PlayerState.PLAYING){
              onStateChangeCb('playing');
            }else if(e.data === YT.PlayerState.PAUSED){
              onStateChangeCb('paused');
            }else if(e.data === YT.PlayerState.ENDED){
              onStateChangeCb('ended');
            }
          }
        }
      });
    }

    return {
      kind: 'youtube',
      init: function(){
        if(window.YT && window.YT.Player){
          attachPlayer();
          return;
        }
        const prevReady = window.onYouTubeIframeAPIReady;
        window.onYouTubeIframeAPIReady = function(){
          if(typeof prevReady === 'function') prevReady();
          attachPlayer();
        };
        if(!scriptAdded){
          scriptAdded = true;
          const tag = document.createElement('script');
          tag.src = 'https://www.youtube.com/iframe_api';
          document.head.appendChild(tag);
        }
      },
      isReady: function(){
        return ready && ytPlayer !== null;
      },
      cue: function(videoId, startSeconds){
        if(!videoId) return;
        const sec = Number(startSeconds) || 0;
        if(ready && ytPlayer && ytPlayer.cueVideoById){
          ytPlayer.cueVideoById({videoId: videoId, startSeconds: sec});
        }else{
          pendingAction = function(){
            if(ytPlayer && ytPlayer.cueVideoById){
              ytPlayer.cueVideoById({videoId: videoId, startSeconds: sec});
            }
          };
        }
      },
      load: function(videoId, startSeconds){
        if(!videoId) return;
        const sec = Number(startSeconds) || 0;
        if(ready && ytPlayer && ytPlayer.loadVideoById){
          ytPlayer.loadVideoById({videoId: videoId, startSeconds: sec});
        }else{
          pendingAction = function(){
            if(ytPlayer && ytPlayer.loadVideoById){
              ytPlayer.loadVideoById({videoId: videoId, startSeconds: sec});
            }
          };
        }
      },
      play: function(){
        if(ready && ytPlayer && ytPlayer.playVideo){
          ytPlayer.playVideo();
        }
      },
      pause: function(){
        pendingAction = null;
        if(ready && ytPlayer && ytPlayer.pauseVideo){
          ytPlayer.pauseVideo();
        }
      },
      seekTo: function(seconds){
        if(ready && ytPlayer && ytPlayer.seekTo){
          ytPlayer.seekTo(seconds, true);
        }
      },
      setVolume: function(vol){
        pendingVolume = vol;
        if(ready && ytPlayer && ytPlayer.setVolume){
          ytPlayer.setVolume(vol);
        }
      },
      getCurrentTime: function(){
        if(ready && ytPlayer && ytPlayer.getCurrentTime){
          return ytPlayer.getCurrentTime() || 0;
        }
        return 0;
      },
      getDuration: function(){
        if(ready && ytPlayer && ytPlayer.getDuration){
          return ytPlayer.getDuration() || 0;
        }
        return 0;
      },
      prefetch: function(){},
      unlock: function(){},
      setVideoMode: function(){}
    };
  }

  function createAudioStreamAdapter(opts){
    const onReadyCb = (opts && opts.onReady) || function(){};
    const onStateChangeCb = (opts && opts.onStateChange) || function(){};

    const audio = document.createElement('audio');
    audio.preload = 'auto';
    audio.playsInline = true;
    audio.setAttribute('playsinline', '');
    audio.style.display = 'none';

    const SILENT_WAV = 'data:audio/wav;base64,UklGRigAAABXQVZFZm10IBIAAAABAAEARKwAAIhYAQACABAAAABkYXRhAgAAAAEA';
    let ready = false;
    let unlocked = false;
    let priming = false;
    let loadingInFlight = false;
    let cuedVideoId = null;
    let loadedVideoId = null;
    let pendingSeek = 0;
    let consecutiveErrors = 0;
    let retryTimer = null;
    const prefetched = new Set();

    function clearRetry(){
      if(retryTimer){
        clearTimeout(retryTimer);
        retryTimer = null;
      }
    }

    function warmStream(videoId){
      if(!videoId || prefetched.has(videoId)) return;
      prefetched.add(videoId);
      fetch('/api/stream/' + encodeURIComponent(videoId) + '?prefetch=1', {
        method: 'GET',
        credentials: 'same-origin'
      }).catch(function(){
        prefetched.delete(videoId);
      });
    }

    audio.addEventListener('loadedmetadata', function(){
      if(priming) return;
      if(pendingSeek > 0){
        try { audio.currentTime = pendingSeek; } catch(e){}
        pendingSeek = 0;
      }
    });
    audio.addEventListener('loadstart', function(){
      if(priming) return;
      onStateChangeCb('loading');
    });
    audio.addEventListener('waiting', function(){
      if(priming) return;
      onStateChangeCb('loading');
    });
    audio.addEventListener('stalled', function(){
      if(priming || audio.paused) return;
      onStateChangeCb('loading');
    });
    audio.addEventListener('playing', function(){
      if(priming) return;
      clearRetry();
      loadingInFlight = false;
      consecutiveErrors = 0;
      onStateChangeCb('playing');
    });
    audio.addEventListener('pause', function(){
      if(priming || loadingInFlight) return;
      if(!audio.ended){
        onStateChangeCb('paused');
      }
    });
    audio.addEventListener('ended', function(){
      if(priming) return;
      clearRetry();
      loadingInFlight = false;
      onStateChangeCb('ended');
    });
    audio.addEventListener('error', function(){
      if(priming) return;
      clearRetry();
      loadingInFlight = false;
      consecutiveErrors++;
      if(cuedVideoId && consecutiveErrors <= 2){
        const resumeAt = isFinite(audio.currentTime) && audio.currentTime > 1 ? audio.currentTime : pendingSeek;
        const vid = cuedVideoId;
        console.warn('audio stream transient error on', vid, 'retrying at', resumeAt, '(' + consecutiveErrors + '/2)...');
        onStateChangeCb('loading');
        retryTimer = setTimeout(function(){
          retryTimer = null;
          if(cuedVideoId !== vid) return;
          loadedVideoId = vid;
          pendingSeek = resumeAt;
          audio.src = '/api/stream/' + encodeURIComponent(vid) + '?r=' + consecutiveErrors;
          audio.load();
          const p = audio.play();
          if(p && typeof p.catch === 'function'){
            p.catch(function(){});
          }
        }, 600 * consecutiveErrors);
      }else if(cuedVideoId && consecutiveErrors <= 4){
        console.warn('audio stream error on', cuedVideoId, 'advancing (' + consecutiveErrors + '/4)...');
        setTimeout(function(){ onStateChangeCb('ended'); }, 1000);
      }else{
        onStateChangeCb('paused');
      }
    });

    return {
      kind: 'audio',
      init: function(){
        if(!audio.parentNode){
          document.body.appendChild(audio);
        }
        ready = true;
        setTimeout(onReadyCb, 0);
      },
      isReady: function(){
        return ready;
      },
      unlock: function(){
        if(unlocked || loadedVideoId) return;
        unlocked = true;
        priming = true;
        audio.src = SILENT_WAV;
        const p = audio.play();
        if(p && typeof p.then === 'function'){
          p.then(function(){
            if(priming){
              audio.pause();
              priming = false;
            }
          }).catch(function(){
            priming = false;
          });
        }else{
          priming = false;
        }
      },
      cue: function(videoId, startSeconds){
        cuedVideoId = videoId;
        if(Number(startSeconds) > 0){
          pendingSeek = Number(startSeconds);
        }
        warmStream(videoId);
      },
      load: function(videoId, startSeconds){
        clearRetry();
        consecutiveErrors = 0;
        unlocked = true;
        priming = false;
        loadingInFlight = true;
        const sec = Number(startSeconds) || 0;
        const sameInFlight = (loadedVideoId === videoId && audio.readyState < 2 && !audio.paused);
        cuedVideoId = videoId;
        prefetched.add(videoId);
        onStateChangeCb('loading');
        if(!sameInFlight){
          loadedVideoId = videoId;
          pendingSeek = sec;
          audio.src = '/api/stream/' + encodeURIComponent(videoId);
          audio.load();
        }else if(sec > 0){
          try { audio.currentTime = sec; } catch(e){}
        }
        const p = audio.play();
        if(p && typeof p.catch === 'function'){
          p.catch(function(err){
            if(err && err.name === 'AbortError') return;
            loadingInFlight = false;
            console.warn('audio play requires user gesture:', err);
            onStateChangeCb('paused');
          });
        }
      },
      play: function(){
        unlocked = true;
        priming = false;
        loadingInFlight = true;
        if(cuedVideoId && loadedVideoId !== cuedVideoId){
          loadedVideoId = cuedVideoId;
          onStateChangeCb('loading');
          audio.src = '/api/stream/' + encodeURIComponent(cuedVideoId);
          audio.load();
        }else if(audio.readyState < 3){
          onStateChangeCb('loading');
        }
        const p = audio.play();
        if(p && typeof p.catch === 'function'){
          p.catch(function(err){
            if(err && err.name === 'AbortError') return;
            loadingInFlight = false;
            console.warn('audio play blocked:', err);
            onStateChangeCb('paused');
          });
        }
      },
      pause: function(){
        clearRetry();
        loadingInFlight = false;
        audio.pause();
        onStateChangeCb('paused');
      },
      seekTo: function(seconds){
        if(isFinite(seconds) && seconds >= 0){
          if(audio.readyState >= 1){
            try { audio.currentTime = seconds; } catch(e){}
          }else{
            pendingSeek = seconds;
          }
        }
      },
      setVolume: function(vol){
        const v = Math.max(0, Math.min(100, Number(vol) || 0)) / 100;
        audio.volume = v;
      },
      getCurrentTime: function(){
        return isFinite(audio.currentTime) ? audio.currentTime : 0;
      },
      getDuration: function(){
        return isFinite(audio.duration) ? audio.duration : 0;
      },
      prefetch: function(videoId){
        warmStream(videoId);
      },
      setVideoMode: function(){}
    };
  }

  function createHybridAdapter(opts){
    const onReadyCb = (opts && opts.onReady) || function(){};
    const onStateChangeCb = (opts && opts.onStateChange) || function(){};

    let videoMode = false;
    let ytInited = false;

    const audioAdapter = createAudioStreamAdapter({
      onReady: function(){
        if(!videoMode) onReadyCb();
      },
      onStateChange: function(status){
        if(!videoMode) onStateChangeCb(status);
      }
    });

    const ytAdapter = createYouTubeAdapter({
      elementId: opts && opts.elementId,
      onReady: function(){
        if(videoMode) onReadyCb();
      },
      onStateChange: function(status){
        if(videoMode) onStateChangeCb(status);
      }
    });

    function active(){
      return videoMode ? ytAdapter : audioAdapter;
    }

    return {
      get kind(){
        return videoMode ? 'youtube' : 'audio';
      },
      init: function(){
        audioAdapter.init();
      },
      isReady: function(){
        return videoMode ? (ytAdapter.isReady() || audioAdapter.isReady()) : audioAdapter.isReady();
      },
      unlock: function(){
        audioAdapter.unlock();
      },
      cue: function(videoId){
        audioAdapter.cue(videoId);
        if(videoMode){
          if(!ytInited){ ytInited = true; ytAdapter.init(); }
          ytAdapter.cue(videoId);
        }
      },
      load: function(videoId){
        if(videoMode){
          audioAdapter.pause();
          audioAdapter.cue(videoId);
          if(!ytInited){ ytInited = true; ytAdapter.init(); }
          ytAdapter.load(videoId);
        }else{
          if(ytInited) ytAdapter.pause();
          audioAdapter.load(videoId);
        }
      },
      play: function(){
        if(videoMode && !ytInited){
          ytInited = true;
          ytAdapter.init();
        }
        active().play();
      },
      pause: function(){
        audioAdapter.pause();
        if(ytInited) ytAdapter.pause();
      },
      seekTo: function(seconds){
        active().seekTo(seconds);
      },
      setVolume: function(vol){
        audioAdapter.setVolume(vol);
        ytAdapter.setVolume(vol);
      },
      getCurrentTime: function(){
        return active().getCurrentTime();
      },
      getDuration: function(){
        return active().getDuration();
      },
      prefetch: function(videoId){
        audioAdapter.prefetch(videoId);
      },
      setVideoMode: function(enabled, currentVideoId, shouldPlay){
        const nextMode = Boolean(enabled);
        if(nextMode === videoMode) return;
        const resumeAt = active().getCurrentTime() || 0;
        videoMode = nextMode;
        if(videoMode){
          audioAdapter.pause();
          if(!ytInited){
            ytInited = true;
            ytAdapter.init();
          }
          if(currentVideoId){
            if(shouldPlay){
              ytAdapter.load(currentVideoId, resumeAt);
            }else{
              ytAdapter.cue(currentVideoId, resumeAt);
            }
          }
        }else{
          if(ytInited) ytAdapter.pause();
          if(currentVideoId){
            if(shouldPlay){
              audioAdapter.load(currentVideoId, resumeAt);
            }else{
              audioAdapter.cue(currentVideoId, resumeAt);
            }
          }
        }
      }
    };
  }

  function createAutoAdapter(opts){
    if(isAppleMobile()){
      return createHybridAdapter(opts);
    }
    return createYouTubeAdapter(opts);
  }

  window.StationPlayer = {
    isAppleMobile: isAppleMobile,
    createYouTubeAdapter: createYouTubeAdapter,
    createAudioStreamAdapter: createAudioStreamAdapter,
    createHybridAdapter: createHybridAdapter,
    createAutoAdapter: createAutoAdapter
  };
})();
