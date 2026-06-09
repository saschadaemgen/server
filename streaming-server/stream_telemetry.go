package stream

import "sort"

// Stream telemetry (S20): the read-only consumer-count surface for the
// carvilon admin's cloud-viewer panel, the exact egress mirror of the TURN
// telemetry in turn_telemetry.go. Where TURNStats reports the relay's
// allocations, StreamStats reports how many WHEP subscribers (consumers) the
// cloud fan-out is serving per stream.
//
// OPEN-CORE rule (same as TURNStats / streampublish.ICEServer at the seam):
// these types carry ONLY stdlib values - here a string streamID and an int
// count. No pion type and no net.Addr crosses the seam, so the embedding
// module's public build stays pion-free.
//
// The streamID is the per-viewer MAC the WHIP/WHEP routes by (the same stable
// identity the side-channel Envelope.StreamID carries), so the edge resolves
// each entry to a viewer/profile without any extra mapping.

// StreamStat is the live WHEP-subscriber (consumer) count for one stream on
// the cloud fan-out. Consumers is the number of WHEP PeerConnections currently
// attached to that streamID's publisher track.
type StreamStat struct {
	StreamID  string // the viewer MAC the WHIP/WHEP routes by
	Consumers int    // live WHEP subscribers attached to this stream
}

// streamStatsFromCounts converts the whip.Server's live per-streamID consumer
// counts into the sorted, stdlib-only []StreamStat the cloud pushes to the
// edge. Only streams with at least one consumer appear (the whip counter drops
// a stream's key at zero). Sorted by StreamID so the snapshot and its
// verification log are deterministic.
func streamStatsFromCounts(counts map[string]int) []StreamStat {
	if len(counts) == 0 {
		return nil
	}
	out := make([]StreamStat, 0, len(counts))
	for id, n := range counts {
		out = append(out, StreamStat{StreamID: id, Consumers: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StreamID < out[j].StreamID })
	return out
}
