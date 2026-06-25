package designer

import (
	"strings"

	"carvilon.local/server/internal/engine"
)

// CatalogPort is one input or output of a building block in the
// designer catalog. Kind is "bool" | "float" | "text". Optional is only
// meaningful on inputs (omitted when false / on outputs).
type CatalogPort struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Optional bool   `json:"optional,omitempty"`
}

// CatalogParam is one configuration parameter of a building block.
// Default carries the value in the param's kind domain (bool/number/
// string).
type CatalogParam struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Default any    `json:"default"`
}

// CatalogBlock is one designer-catalog entry: the building blocks the
// editor palette offers, in five categories. Implemented marks the four
// blocks backed by a real engine node (input.manual, time.staircase,
// logic.or, output.lamp); their ports/params/delay-boundary are derived
// from the engine registry so they stay the single source of truth. The
// rest are catalog-only for now (no ports/params yet) — see Catalog.
type CatalogBlock struct {
	Type          string         `json:"type"`
	Category      string         `json:"category"`
	Title         string         `json:"title"`
	Icon          string         `json:"icon"`
	Inputs        []CatalogPort  `json:"inputs"`
	Outputs       []CatalogPort  `json:"outputs"`
	Params        []CatalogParam `json:"params"`
	DelayBoundary bool           `json:"delay_boundary"`
	Implemented   bool           `json:"implemented"`
}

// Catalog returns the 111 building-block descriptors the editor palette
// renders, in the five-category order the editor expects (input, logic,
// time, memory, output). The four implemented blocks have their ports,
// params and delay-boundary filled from the engine registry; the other
// 107 are catalog-only (empty ports/params) until their engine nodes
// land. The httpserver serializes this to /a/designer/catalog.json.
func Catalog() []CatalogBlock {
	blocks := rawBlocks()
	for i := range blocks {
		b := &blocks[i]
		if b.Implemented {
			if d, ok := engine.Lookup(b.Type); ok {
				b.Inputs = portsToCatalog(d.Inputs)
				b.Outputs = portsToCatalog(d.Outputs)
				b.Params = paramsToCatalog(d.Params)
				b.DelayBoundary = d.DelayBoundary
			}
		}
		if b.Inputs == nil {
			b.Inputs = []CatalogPort{}
		}
		if b.Outputs == nil {
			b.Outputs = []CatalogPort{}
		}
		if b.Params == nil {
			b.Params = []CatalogParam{}
		}
	}
	return blocks
}

// rawBlocks builds the catalog skeleton (type/category/title/icon, and
// the implemented flag) ported 1:1 from the editor's former inline block
// list. add() derives a stable category.slug type; impl() pins the
// engine type for the four implemented blocks.
func rawBlocks() []CatalogBlock {
	var out []CatalogBlock
	add := func(cat, title, icon string) {
		out = append(out, CatalogBlock{Type: cat + "." + slug(title), Category: cat, Title: title, Icon: icon})
	}
	impl := func(cat, title, icon, typ string) {
		out = append(out, CatalogBlock{Type: typ, Category: cat, Title: title, Icon: icon, Implemented: true})
	}

	// input (26)
	impl("input", "Push-button", "circle-dot", "input.manual")
	add("input", "Dual button", "copy")
	add("input", "Motion sensor", "radar")
	add("input", "Presence sensor", "user-check")
	add("input", "Switch", "toggle-left")
	add("input", "Wall switch", "square-mouse-pointer")
	add("input", "Door sensor", "door-open")
	add("input", "Window contact", "app-window")
	add("input", "Smoke detector", "flame")
	add("input", "Water sensor", "droplets")
	add("input", "Temperature", "thermometer")
	add("input", "Brightness", "sun")
	add("input", "Wind sensor", "wind")
	add("input", "Rain sensor", "cloud-rain")
	add("input", "Touch", "fingerprint")
	add("input", "NFC reader", "radio")
	add("input", "Key switch", "key-round")
	add("input", "Doorbell", "bell")
	add("input", "Analog input", "sliders-horizontal")
	add("input", "Counter input", "hash")
	add("input", "CO₂ sensor", "wind")
	add("input", "Humidity", "droplet")
	add("input", "Vibration", "vibrate")
	add("input", "Frost guard", "snowflake")
	add("input", "Dew point", "thermometer-snowflake")
	add("input", "Motion Air", "radar")

	// logic (26)
	add("logic", "AND", "ampersand")
	impl("logic", "OR", "git-merge", "logic.or")
	add("logic", "NOT", "slash")
	add("logic", "XOR", "shuffle")
	add("logic", "NAND", "ampersand")
	add("logic", "NOR", "git-merge")
	add("logic", "Flip-flop", "toggle-right")
	add("logic", "Comparator", "equal")
	add("logic", "Multiplexer", "split")
	add("logic", "Formula", "function-square")
	add("logic", "Status select", "list-checks")
	add("logic", "Counter", "hash")
	add("logic", "Threshold", "gauge")
	add("logic", "Hysteresis", "activity")
	add("logic", "Min/Max", "arrow-up-down")
	add("logic", "Limiter", "brackets")
	add("logic", "Gate", "door-closed")
	add("logic", "Interlock", "lock")
	add("logic", "State machine", "workflow")
	add("logic", "Edge", "triangle")
	add("logic", "Math", "calculator")
	add("logic", "Constant", "pi")
	add("logic", "Adder", "plus")
	add("logic", "Multiplier", "x")
	add("logic", "Selector", "list")
	add("logic", "Binary decoder", "binary")

	// time (22)
	impl("time", "Staircase light", "timer", "time.staircase")
	add("time", "On-delay", "timer-reset")
	add("time", "Off-delay", "timer-off")
	add("time", "Pulse", "activity")
	add("time", "Clock gen.", "square-activity")
	add("time", "Weekly timer", "calendar-days")
	add("time", "Yearly timer", "calendar")
	add("time", "Astro clock", "sunrise")
	add("time", "Timer", "timer")
	add("time", "Countdown", "timer")
	add("time", "Blinker", "zap")
	add("time", "Sequencer", "list-ordered")
	add("time", "Interval", "repeat")
	add("time", "Schedule", "clock")
	add("time", "Sun position", "sun")
	add("time", "Wipe relay", "zap")
	add("time", "Run-on", "history")
	add("time", "Stopwatch", "timer")
	add("time", "Delay", "hourglass")
	add("time", "Pulse width", "audio-waveform")
	add("time", "Daylight", "sun-medium")
	add("time", "Holiday mode", "plane")

	// memory (13)
	add("memory", "Flag", "bookmark")
	add("memory", "Status block", "database")
	add("memory", "Memory", "save")
	add("memory", "Buffer", "layers")
	add("memory", "Counter value", "hash")
	add("memory", "Value store", "box")
	add("memory", "Shift register", "move-horizontal")
	add("memory", "Stack", "layers")
	add("memory", "Logbook", "scroll-text")
	add("memory", "Variable", "variable")
	add("memory", "Buffer store", "container")
	add("memory", "Ring buffer", "rotate-cw")
	add("memory", "Constant store", "lock")

	// output (24)
	impl("output", "Lamp", "lightbulb", "output.lamp")
	add("output", "Dimmer", "sun-dim")
	add("output", "Relay", "toggle-right")
	add("output", "Blind", "blinds")
	add("output", "Shutter", "panel-top")
	add("output", "Awning", "umbrella")
	add("output", "Valve", "git-commit-vertical")
	add("output", "Socket", "plug")
	add("output", "Heating", "flame")
	add("output", "Fan", "fan")
	add("output", "RGB light", "palette")
	add("output", "RGBW light", "palette")
	add("output", "Door opener", "door-open")
	add("output", "Siren", "siren")
	add("output", "Motor", "cog")
	add("output", "Pump", "droplets")
	add("output", "Garage door", "warehouse")
	add("output", "Audio zone", "volume-2")
	add("output", "Scene", "clapperboard")
	add("output", "Analog output", "sliders-horizontal")
	add("output", "Constant light", "sun")
	add("output", "Wallbox", "plug-zap")
	add("output", "Boiler", "flame")
	add("output", "Gate", "fence")

	return out
}

// slug reduces a title to its lowercase alphanumerics, used to build a
// stable per-category type id for the not-yet-implemented blocks.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func portsToCatalog(ports []engine.Port) []CatalogPort {
	out := make([]CatalogPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, CatalogPort{Name: p.Name, Kind: kindString(p.Kind), Optional: p.Optional})
	}
	return out
}

func paramsToCatalog(params []engine.Param) []CatalogParam {
	out := make([]CatalogParam, 0, len(params))
	for _, p := range params {
		out = append(out, CatalogParam{Name: p.Name, Kind: kindString(p.Kind), Default: valueScalar(p.Default)})
	}
	return out
}

// kindString maps an engine value kind to the catalog's string form.
func kindString(k engine.Kind) string {
	switch k {
	case engine.Bool:
		return "bool"
	case engine.Float:
		return "float"
	case engine.Text:
		return "text"
	default:
		return "bool"
	}
}

// valueScalar extracts the meaningful scalar from a tagged engine Value
// so a param default serializes as a plain bool/number/string.
func valueScalar(v engine.Value) any {
	switch v.Kind {
	case engine.Bool:
		return v.B
	case engine.Float:
		return v.F
	case engine.Text:
		return v.S
	default:
		return nil
	}
}
