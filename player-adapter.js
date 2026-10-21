/* player-adapter.js — isolates playback transport behind a uniform PlayerAdapter contract.
 *
 * Implementations:
 *   - createYouTubeAdapter: YouTube IFrame API (desktop default, supports video + audio).
 *   - createAudioStreamAdapter: HTML5 <audio> streaming /api/stream/{videoId} (iPad/iPhone Safari
 *     default; supports lock-screen background playback, Control Center MediaSession, and
 *     uninterrupted multi-track auto-advance without user re-tap).
 *   - createAutoAdapter: selects AudioStreamAdapter on iPadOS/iOS (or ?player=audio) and
 *     YouTubeAdapter on desktop.
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

    window.onYouTubeIframeAPIReady = function(){
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
          origin: window.location.origin,
          widget_referrer: window.location.origin
        },
        events: {
          onReady: function(){
            ready = true;
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
    };

    return {
      kind: 'youtube',
      init: function(){
        const tag = document.createElement('script');
        tag.src = 'https://www.youtube.com/iframe_api';
        document.head.appendChild(tag);
      },
      isReady: function(){
        return ready && ytPlayer !== null;
      },
      cue: function(videoId){
        if(ready && ytPlayer && ytPlayer.cueVideoById){
          ytPlayer.cueVideoById(videoId);
        }
      },
      load: function(videoId){
        if(ready && ytPlayer && ytPlayer.loadVideoById){
          ytPlayer.loadVideoById(videoId);
        }
      },
      play: function(){
        if(ready && ytPlayer && ytPlayer.playVideo){
          ytPlayer.playVideo();
        }
      },
      pause: function(){
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
      unlock: function(){}
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
    let cuedVideoId = null;
    let loadedVideoId = null;
    let consecutiveErrors = 0;
    const prefetched = new Set();

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

    audio.addEventListener('loadstart', function(){
      if(priming) return;
      onStateChangeCb('loading');
    });
    audio.addEventListener('waiting', function(){
      if(priming) return;
      onStateChangeCb('loading');
    });
    audio.addEventListener('playing', function(){
      if(priming) return;
      consecutiveErrors = 0;
      onStateChangeCb('playing');
    });
    audio.addEventListener('pause', function(){
      if(priming) return;
      if(!audio.ended){
        onStateChangeCb('paused');
      }
    });
    audio.addEventListener('ended', function(){
      if(priming) return;
      onStateChangeCb('ended');
    });
    audio.addEventListener('error', function(){
      if(priming) return;
      consecutiveErrors++;
      if(cuedVideoId && consecutiveErrors <= 3){
        console.warn('audio stream error on', cuedVideoId, 'advancing (' + consecutiveErrors + '/3)...');
        setTimeout(function(){ onStateChangeCb('ended'); }, 1200);
      }else{
        onStateChangeCb('paused');
      }
    });

    return {
      kind: 'audio',
      init: function(){
        document.body.appendChild(audio);
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
      cue: function(videoId){
        cuedVideoId = videoId;
        warmStream(videoId);
      },
      load: function(videoId){
        unlocked = true;
        priming = false;
        const sameInFlight = (loadedVideoId === videoId && audio.readyState < 2 && !audio.paused);
        cuedVideoId = videoId;
        prefetched.add(videoId);
        onStateChangeCb('loading');
        if(!sameInFlight){
          loadedVideoId = videoId;
          audio.src = '/api/stream/' + encodeURIComponent(videoId);
          audio.load();
        }
        const p = audio.play();
        if(p && typeof p.catch === 'function'){
          p.catch(function(err){
            if(err && err.name === 'AbortError') return;
            console.warn('audio play requires user gesture:', err);
            onStateChangeCb('paused');
          });
        }
      },
      play: function(){
        unlocked = true;
        priming = false;
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
            console.warn('audio play blocked:', err);
            onStateChangeCb('paused');
          });
        }
      },
      pause: function(){
        audio.pause();
        onStateChangeCb('paused');
      },
      seekTo: function(seconds){
        if(isFinite(seconds) && seconds >= 0){
          try { audio.currentTime = seconds; } catch(e){}
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
      }
    };
  }

  function createAutoAdapter(opts){
    if(isAppleMobile()){
      return createAudioStreamAdapter(opts);
    }
    return createYouTubeAdapter(opts);
  }

  window.StationPlayer = {
    isAppleMobile: isAppleMobile,
    createYouTubeAdapter: createYouTubeAdapter,
    createAudioStreamAdapter: createAudioStreamAdapter,
    createAutoAdapter: createAutoAdapter
  };
})();
