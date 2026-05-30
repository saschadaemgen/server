//go:build carvilon_stream

// Saison 15: commercial build binds the private streaming-server
// behind the StreamBackend seam. Build with -tags carvilon_stream
// (requires private-module access).
package streams

import (
	"context"
	"errors"

	private "carvilon.local/stream/streambackend"
)

// CarvilonStreamBackend wraps the private streaming-server's
// streambackend.Backend so it satisfies the public StreamBackend
// interface. Field-by-field conversion only - the wire shapes match
// 1:1 because both sides committed to the same JSON tags.
type CarvilonStreamBackend struct {
	b *private.Backend
}

// NewCarvilonStreamBackend builds the wrapper. The caller (main.go)
// supplies the constructed private.Backend; this file only does the
// type translation.
func NewCarvilonStreamBackend(b *private.Backend) *CarvilonStreamBackend {
	return &CarvilonStreamBackend{b: b}
}

// Compile-time sanity: the wrapper satisfies the public interface.
var _ StreamBackend = (*CarvilonStreamBackend)(nil)

func (w *CarvilonStreamBackend) MJPEGURL(profile string) string {
	return w.b.MJPEGURL(profile)
}

func (w *CarvilonStreamBackend) WebRTCSignalURL(profile string) string {
	return w.b.WebRTCSignalURL(profile)
}

func (w *CarvilonStreamBackend) List(ctx context.Context) ([]Profile, error) {
	rows, err := w.b.List(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Profile, len(rows))
	for i, p := range rows {
		out[i] = fromPrivateProfile(p)
	}
	return out, nil
}

func (w *CarvilonStreamBackend) Get(ctx context.Context, name string) (Profile, error) {
	p, err := w.b.Get(ctx, name)
	if err != nil {
		return Profile{}, mapErr(err)
	}
	return fromPrivateProfile(p), nil
}

func (w *CarvilonStreamBackend) Put(ctx context.Context, p Profile) error {
	return mapErr(w.b.Put(ctx, toPrivateProfile(p)))
}

func (w *CarvilonStreamBackend) Delete(ctx context.Context, name string) error {
	return mapErr(w.b.Delete(ctx, name))
}

func (w *CarvilonStreamBackend) ListCameras(ctx context.Context) ([]Camera, error) {
	cams, err := w.b.ListCameras(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Camera, len(cams))
	for i, c := range cams {
		out[i] = Camera{
			ID:            c.ID,
			Name:          c.Name,
			Online:        c.Online,
			HasPackageCam: c.HasPackageCam,
		}
	}
	return out, nil
}

// Stats synthesises the public ProfileStats map from the private
// backend's List result. S2-06 removed the separate Backend.Stats
// method and moved the live consumer count into Profile.Consumers
// (filled by List/Get from the hub state), so the only telemetry the
// in-process backend exposes is Clients == Consumers. The richer
// fields (AvgFPS / SourceFPS / AvgBitrateKbps) the HTTP /stream/stats
// client reports are not available here and stay zero; the admin list
// only renders the Clients/Consumers column.
func (w *CarvilonStreamBackend) Stats(ctx context.Context) (map[string]ProfileStats, error) {
	rows, err := w.b.List(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make(map[string]ProfileStats, len(rows))
	for _, p := range rows {
		out[p.Name] = ProfileStats{
			Profile: p.Name,
			Clients: p.Consumers,
		}
	}
	return out, nil
}

func (w *CarvilonStreamBackend) Configured() bool { return w.b.Configured() }

func fromPrivateProfile(p private.Profile) Profile {
	return Profile{
		Name:          p.Name,
		CameraID:      p.CameraID,
		Quality:       p.Quality,
		Usage:         p.Usage,
		Description:   p.Description,
		Codec:         p.Codec,
		Width:         p.Width,
		Height:        p.Height,
		FPS:           p.FPS,
		EncodeQuality: p.EncodeQuality,
		Encryption:    p.Encryption,
	}
}

func toPrivateProfile(p Profile) private.Profile {
	return private.Profile{
		Name:          p.Name,
		CameraID:      p.CameraID,
		Quality:       p.Quality,
		Usage:         p.Usage,
		Description:   p.Description,
		Codec:         p.Codec,
		Width:         p.Width,
		Height:        p.Height,
		FPS:           p.FPS,
		EncodeQuality: p.EncodeQuality,
		Encryption:    p.Encryption,
	}
}

// mapErr translates the private sentinel errors to the public ones so
// admin handlers errors.Is(err, ErrProfileNotFound) keep working.
// Saison-15-07-Nachtrag-Anpassung: the stream-repo's hand-written
// errorsIs was replaced by stdlib errors.Is per dependency-doctrine
// (stdlib first).
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, private.ErrProfileNotFound) {
		return ErrProfileNotFound
	}
	if errors.Is(err, private.ErrNotConfigured) {
		return ErrNotConfigured
	}
	return err
}
