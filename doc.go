// Package stream is the CARVILON streaming-server library.
//
// Scope (S1 Spike, Schritt 1): pull H.264 video from an RTSPS source and
// forward it to a single WebRTC viewer in the browser. No transcoding, no
// fan-out, no audio.
//
// The library is structured so it can later be moved into carvilon-server
// as a package — a relocation, not a rewrite. The public surface today is
// deliberately small (a [Source] that owns the RTSP pull, and a [Server]
// that exposes WebRTC signaling) so a future VideoSource interface can be
// lifted above it without churning callers.
package stream
