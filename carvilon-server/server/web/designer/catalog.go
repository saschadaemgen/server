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
	// Channel and Unit are set on the data-driven system blocks: Channel
	// is the fixed physical ref (e.g. "sys:cpu_temp") the editor bakes into
	// the dropped node, Unit the display suffix (°C, %). Omitted elsewhere.
	Channel string `json:"channel,omitempty"`
	Unit    string `json:"unit,omitempty"`
}

// SysMetric is one available system-telemetry metric the catalog turns
// into a source block (data-driven from the sys: driver).
type SysMetric struct {
	Address string // full physical ref, e.g. "sys:cpu_temp"
	Label   string
	Unit    string
}

// NFCReader is one detected NFC tag reader the catalog turns into two
// source blocks (data-driven from the nfc: driver): the tag UID as a
// text source and "Tag da" as a bool source, each with its physical ref
// baked in.
type NFCReader struct {
	ID             string // stable reader id, e.g. "i2c-1"
	UIDChannel     string // full physical ref, e.g. "nfc:i2c-1:uid"
	PresentChannel string // full physical ref, e.g. "nfc:i2c-1:present"
}

// Catalog returns the building-block descriptors the editor palette
// renders: the 111 base blocks in the five-category order (input, logic,
// time, memory, output), plus the runtime-detected driver categories -
// "gpio" on a GPIO host, "system" where telemetry is readable, "nfc"
// when a tag reader is detected, "mqtt"/"telegram" while broker/bot run.
// The implemented blocks have their ports, params and delay-boundary
// filled from the engine registry; the catalog-only ones have empty
// ports/params until their engine nodes land. The httpserver passes the
// runtime flags and serializes this to /a/designer/catalog.json.
func Catalog(includeGPIO bool, sysMetrics []SysMetric, nfcReaders []NFCReader, includeMQTT, includeTelegram bool) []CatalogBlock {
	blocks := rawBlocks()
	if includeGPIO {
		blocks = append(blocks, gpioBlocks()...)
	}
	if len(sysMetrics) > 0 {
		blocks = append(blocks, sysBlocks(sysMetrics)...)
	}
	if len(nfcReaders) > 0 {
		blocks = append(blocks, nfcBlocks(nfcReaders)...)
	}
	if includeMQTT {
		blocks = append(blocks, mqttBlocks()...)
	}
	if includeTelegram {
		blocks = append(blocks, telegramBlocks()...)
	}
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

// gpioBlocks are the two generic GPIO palette blocks, appended to the
// catalog only on GPIO-capable hosts (Catalog's includeGPIO, driven by
// runtime detection). They are typed to the engine's source.channel /
// sink.channel nodes, so the implemented-fill loop derives their ports
// and params from the registry; the concrete line (chip:offset) is the
// user-set "channel" param, bound to a gpio: address at run time. Not
// per-pin: one generic input and one generic output, host-agnostic.
func gpioBlocks() []CatalogBlock {
	return []CatalogBlock{
		{Type: engine.TypeSourceChannel, Category: "gpio", Title: "GPIO Eingang", Icon: "import", Implemented: true},
		{Type: engine.TypeSinkChannel, Category: "gpio", Title: "GPIO Ausgang", Icon: "plug-zap", Implemented: true},
	}
}

// mqttBlocks are the two generic MQTT palette blocks, appended only when
// the broker is running (Catalog's includeMQTT). Unlike GPIO (a finite
// line pool) or sys (fixed metrics), MQTT topics are free text, so these
// are two generic blocks: the editor adds a Topic field, a value-type
// selector (which re-types the node to source/sink.channel.<kind>), and
// a Retain switch on the sink. The seed type is the float variant; the
// editor switches it from the value-type selector. Channel is set by the
// editor to "mqtt:<topic>" and bound to the mqtt: driver at run time.
func mqttBlocks() []CatalogBlock {
	return []CatalogBlock{
		{Type: engine.TypeSourceChannelFloat, Category: "mqtt", Title: "MQTT In", Icon: "import", Implemented: true},
		{Type: engine.TypeSinkChannelFloat, Category: "mqtt", Title: "MQTT Out", Icon: "send", Implemented: true},
	}
}

// telegramBlocks are the four Telegram palette blocks, appended only
// when the bot is running (Catalog's includeTelegram: enabled + token
// set). All four are typed to the generic engine channel nodes, so the
// implemented-fill loop derives their ports/params from the registry;
// the editor adds the chat picker (fed by the allowlist), the command
// word, and the fixed message, and serializes the channel param as
// "telegram:<role>:<payload>#<node-id>" - bound to the telegram:
// driver at run time. The two demo flows ride on the first two blocks:
// doorbell -> phone (Senden) and phone -> lamp (Befehl).
func telegramBlocks() []CatalogBlock {
	return []CatalogBlock{
		{Type: engine.TypeSinkChannel, Category: "telegram", Title: "Telegram Senden", Icon: "send", Implemented: true},
		{Type: engine.TypeSourceChannel, Category: "telegram", Title: "Telegram Befehl", Icon: "message-circle", Implemented: true},
		{Type: engine.TypeSourceChannelText, Category: "telegram", Title: "Telegram Empfangen", Icon: "import", Implemented: true},
		{Type: engine.TypeSinkChannelText, Category: "telegram", Title: "Telegram Text senden", Icon: "send", Implemented: true},
	}
}

// sysBlocks turns the available system metrics into one source block each
// (data-driven, appended only when the host exposes telemetry). Every
// block is the engine's source.channel.float node with its physical ref
// baked into Channel - the editor drops a ready-to-run telemetry source,
// no picker. Unit drives the live-value display (°C, %).
func sysBlocks(metrics []SysMetric) []CatalogBlock {
	out := make([]CatalogBlock, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, CatalogBlock{
			Type:        engine.TypeSourceChannelFloat,
			Category:    "system",
			Title:       m.Label,
			Icon:        sysIcon(m.Address),
			Channel:     m.Address,
			Unit:        m.Unit,
			Implemented: true,
		})
	}
	return out
}

// nfcBlocks turns the detected NFC readers into two source blocks each
// (data-driven, appended only when the host has a reader): the tag UID
// as a text source - the card shows the last read UID live - and
// "Tag da" as a bool source (tag resting on the module yes/no). Both
// are generic engine channel nodes with the physical ref baked into
// Channel, ready to run, no picker. With more than one reader the
// titles carry the reader id: the editor's palette lookups are
// title-keyed, so titles MUST stay unique across the catalog.
func nfcBlocks(readers []NFCReader) []CatalogBlock {
	out := make([]CatalogBlock, 0, 2*len(readers))
	for _, r := range readers {
		suffix := ""
		if len(readers) > 1 {
			suffix = " (" + r.ID + ")"
		}
		out = append(out,
			CatalogBlock{Type: engine.TypeSourceChannelText, Category: "nfc", Title: "NFC Leser" + suffix, Icon: "nfc", Channel: r.UIDChannel, Implemented: true},
			CatalogBlock{Type: engine.TypeSourceChannel, Category: "nfc", Title: "NFC Tag da" + suffix, Icon: "scan", Channel: r.PresentChannel, Implemented: true},
		)
	}
	return out
}

// sysIcon maps a metric ref to a lucide icon; a generic gauge otherwise.
func sysIcon(addr string) string {
	switch addr {
	case "sys:cpu_temp":
		return "thermometer"
	case "sys:cpu_load":
		return "cpu"
	case "sys:ram":
		return "memory-stick"
	case "sys:disk_root":
		return "hard-drive"
	default:
		return "gauge"
	}
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
