// WebRTC video client for the carvilon webviewer.
//
// Replaces the MJPEG <img> as the primary live-view path. The
// browser POSTs an SDP offer to /webviewer/offer; the server
// proxies it onto the streaming backend (the resolved profile is
// added server-side via ResolveStreamProfile). The backend
// answers with an SDP; we attach the resulting MediaStream to a
// <video> element.
//
// MJPEG remains as a feature-flagged fallback in the markup
// (two siblings: <video data-webrtc-target> and <img
// data-mjpeg-fallback>). On connect:
//   - success: video gets the stream, MJPEG img is hidden via
//     `data-stream-mode="webrtc"` on the slot wrapper.
//   - failure (503 / network / answer parse): we leave the video
//     paused and flip `data-stream-mode="mjpeg"` so the MJPEG
//     fallback takes over.
//
// Teardown discipline (CRITICAL): track.stop() BEFORE pc.close().
// pagehide + beforeunload listeners run disconnect() as a safety
// net so a stale PeerConnection cannot keep the camera mic open
// after the user navigates away. This pattern is non-negotiable
// (Lesson from MQTT capture: UA-Intercom-WebRTC-Teardown ist
// unzuverlaessig, Mock-UI muss eigenes track.stop + pc.close
// machen, see CLAUDE.md sektion 11.9).
(function () {
  'use strict';

  // Module-level state. Only one connection at a time across the
  // page; the slot caller decides when to disconnect before
  // calling connect with a fresh target.
  var current = null; // { pc: RTCPeerConnection, video: HTMLVideoElement,
                      //   stream: MediaStream | null }

  // signalEndpoint is the same-origin proxy that forwards to the
  // streaming backend's /offer endpoint. The server picks the
  // profile from the authenticated session; the browser never
  // sends src=.
  var SIGNAL_ENDPOINT = '/webviewer/offer';

  function setSlotMode(slotEl, mode) {
    if (!slotEl) return;
    slotEl.setAttribute('data-stream-mode', mode);
  }

  // Find the wrapper that carries data-stream-mode. Both the
  // livestream-layer (idle slot) and the ringing-overlay use the
  // same convention.
  function slotFor(videoEl) {
    if (!videoEl) return null;
    var node = videoEl.parentNode;
    while (node && node.nodeType === 1) {
      if (node.hasAttribute && node.hasAttribute('data-stream-slot')) {
        return node;
      }
      node = node.parentNode;
    }
    return null;
  }

  // teardownPC stops every receiver track, then closes the PC.
  // Order matters: stopping tracks BEFORE close ensures the
  // camera / mic on the source side is released cleanly. Calling
  // it on an already-closed PC is a no-op (defensive try/catch
  // because some browsers throw on double-close).
  function teardownPC(pc, stream) {
    if (stream) {
      try {
        var tracks = stream.getTracks();
        for (var i = 0; i < tracks.length; i++) {
          try { tracks[i].stop(); } catch (_) {}
        }
      } catch (_) {}
    }
    if (pc) {
      try {
        var senders = pc.getReceivers();
        for (var j = 0; j < senders.length; j++) {
          try {
            if (senders[j].track) senders[j].track.stop();
          } catch (_) {}
        }
      } catch (_) {}
      try { pc.close(); } catch (_) {}
    }
  }

  // disconnect tears down the current connection (if any) and
  // detaches the video element. Safe to call when nothing is
  // connected.
  function disconnect() {
    if (!current) return;
    var c = current;
    current = null;
    try {
      if (c.video) {
        c.video.pause();
        c.video.srcObject = null;
      }
    } catch (_) {}
    teardownPC(c.pc, c.stream);
    setSlotMode(slotFor(c.video), 'idle');
  }

  // connect performs the SDP-offer roundtrip and attaches the
  // resulting MediaStream to videoEl. If the offer-POST fails
  // (503 / network) or the answer SDP cannot be applied, we
  // resolve quietly into MJPEG-fallback mode without throwing -
  // the slot caller does not need to handle errors itself.
  function connect(videoEl) {
    if (!videoEl) return;
    // If the page already has a connection on a different
    // target, tear it down first - one webviewer, one stream.
    if (current && current.video !== videoEl) {
      disconnect();
    } else if (current && current.video === videoEl) {
      // Already connected to this element: NoOp. The idle-mode
      // setter calls connect() defensively on every entry into
      // livestream; we honor the existing PC so the consumer
      // slot stays warm and we avoid the SDP+ICE+keyframe
      // handshake on every screensaver toggle.
      return;
    }

    var slot = slotFor(videoEl);
    setSlotMode(slot, 'connecting');

    // Conservative RTCPeerConnection config. STUN/TURN is the
    // backend's job (the offer it returns may contain ICE
    // candidates from configured turn servers); the browser side
    // only needs a default config to send the offer.
    var pc;
    try {
      pc = new RTCPeerConnection({
        iceServers: [],
        // Avoid the legacy plan-b semantics.
        bundlePolicy: 'max-bundle',
      });
    } catch (e) {
      setSlotMode(slot, 'mjpeg');
      return;
    }

    var inboundStream = new MediaStream();
    pc.ontrack = function (ev) {
      // Modern browsers populate ev.streams[0]; we also append
      // tracks individually as a safety net for older builds.
      var s = ev.streams && ev.streams[0] ? ev.streams[0] : null;
      if (s) {
        videoEl.srcObject = s;
        inboundStream = s;
      } else if (ev.track) {
        inboundStream.addTrack(ev.track);
        videoEl.srcObject = inboundStream;
      }
      // play() returns a promise that rejects when autoplay is
      // blocked. We swallow because the user typically taps the
      // surface to bring it to life - no point in throwing.
      var p = videoEl.play();
      if (p && p.catch) p.catch(function () {});
      if (current) current.stream = inboundStream;
      setSlotMode(slot, 'webrtc');
    };

    pc.oniceconnectionstatechange = function () {
      if (!pc) return;
      var st = pc.iceConnectionState;
      if (st === 'failed' || st === 'disconnected' || st === 'closed') {
        // Surface failures by flipping back to MJPEG. The auto-
        // teardown still runs on the next disconnect() call.
        setSlotMode(slot, 'mjpeg');
      }
    };

    // We are video-receive-only on the browser side. Adding a
    // receive-only transceiver tells the backend "I just want
    // your camera feed".
    try {
      pc.addTransceiver('video', { direction: 'recvonly' });
    } catch (_) {
      // Older browsers may not support addTransceiver; fall back
      // to addStream-style by relying on the answer-side offer.
    }

    current = { pc: pc, video: videoEl, stream: null };

    pc.createOffer()
      .then(function (offer) { return pc.setLocalDescription(offer); })
      .then(function () {
        if (!pc.localDescription || !pc.localDescription.sdp) {
          throw new Error('no local SDP');
        }
        return fetch(SIGNAL_ENDPOINT, {
          method: 'POST',
          headers: { 'Content-Type': 'application/sdp', 'Accept': 'application/sdp' },
          // Same-origin -> session cookie travels automatically.
          credentials: 'same-origin',
          body: pc.localDescription.sdp,
        });
      })
      .then(function (resp) {
        if (!resp.ok) {
          throw new Error('offer status ' + resp.status);
        }
        return resp.text();
      })
      .then(function (answerSDP) {
        return pc.setRemoteDescription({ type: 'answer', sdp: answerSDP });
      })
      .catch(function (_err) {
        // Anything goes wrong: surrender the slot to the MJPEG
        // fallback. Quiet by default - the operator can see the
        // server-side log if they care.
        setSlotMode(slot, 'mjpeg');
        // Tear down the half-built PC so we do not leak.
        if (current && current.pc === pc) {
          teardownPC(pc, null);
          current = null;
        }
      });
  }

  // isActive reports whether a connection is up (for the
  // host-page lifecycle hooks).
  function isActive() {
    return !!current;
  }

  // Safety net: if the tab is hidden or unloaded, tear down so
  // the backend can free the consumer slot.
  window.addEventListener('pagehide', disconnect);
  window.addEventListener('beforeunload', disconnect);

  window.carvilonWebRTC = {
    connect: connect,
    disconnect: disconnect,
    isActive: isActive,
  };
})();
