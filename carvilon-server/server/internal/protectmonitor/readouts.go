package protectmonitor

import "carvilon.local/server/internal/engine"

// Readout is one present readout of a sensor, for the designer catalog
// bridge: a stable metric token, display label + unit, engine value kind,
// and the fully-formed physical channel ref an editor source node binds to
// ("protect:<sensorID>:<token>").
type Readout struct {
	Token   string
	Label   string
	Unit    string
	Kind    engine.Kind
	Channel string
}

// KindString renders the readout's engine value kind as the catalog's
// string form ("bool" | "float" | "text"), so the httpserver bridge maps to
// designer.ReadoutPort without importing the engine kind vocabulary.
func (r Readout) KindString() string {
	switch r.Kind {
	case engine.Float:
		return "float"
	case engine.Text:
		return "text"
	default:
		return "bool"
	}
}

// DeviceReadouts is one sensor and the readouts it currently reports - the
// neutral, engine-facing shape the designer catalog turns into a
// capability-driven readout-module block. It is vendor-neutral on purpose:
// any future readout source can produce the same shape and get an editor
// block through the same generic path.
type DeviceReadouts struct {
	ID       string
	Name     string
	Model    string
	Online   bool
	Readouts []Readout
}

// Devices returns the current snapshot's sensors as readout devices for the
// catalog. Only sensors that report at least one readout are included, and
// only the metrics actually present (so an editor block exposes exactly the
// ports the device can feed). The channel refs carry the engine.PrefixProtect
// namespace so a dropped source node binds to this monitor's run driver.
func (m *Monitor) Devices() []DeviceReadouts {
	snap := m.Snapshot()
	now := m.now()
	out := make([]DeviceReadouts, 0, len(snap.Sensors))
	for _, s := range snap.Sensors {
		present := readings(s, now)
		if len(present) == 0 {
			continue
		}
		dev := DeviceReadouts{
			ID:     s.ID,
			Name:   s.DisplayName(),
			Model:  s.ModelLabel(),
			Online: s.IsOnline(),
		}
		for _, mi := range metricCatalog {
			if _, ok := present[mi.Token]; !ok {
				continue
			}
			dev.Readouts = append(dev.Readouts, Readout{
				Token:   mi.Token,
				Label:   mi.Label,
				Unit:    mi.Unit,
				Kind:    mi.Kind,
				Channel: engine.PrefixProtect + ":" + s.ID + ":" + mi.Token,
			})
		}
		out = append(out, dev)
	}
	return out
}
