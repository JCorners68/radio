/* player-adapter.js — isolates playback transport behind a uniform PlayerAdapter contract.
 *
 * Default implementation: YouTubeIframeAdapter (desktop / standard browser).
 * Future implementations (e.g. HTMLAudioStreamAdapter for iOS Safari background audio)
 * can be swapped in without touching queue, state, or UI controls in index.html.
 *
 * Adapter contract:
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
 */
(function(){
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
            if(e.data === YT.PlayerState.PLAYING){
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
      }
    };
  }

  window.StationPlayer = {
    createYouTubeAdapter: createYouTubeAdapter
  };
})();
