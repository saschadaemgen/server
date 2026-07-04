package nfc

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// The protocol layer is tested against recorded byte sequences: two
// hand-verified exact frame vectors pin the frame math, the command
// exchanges run over a scripted bus (including the I2C ready-status
// byte the chip prefixes to every read), and corrupted frames must be
// rejected whole. No hardware, no wall clock (sleep is injected).

// Hand-computed reference frames (UM0701-02 §6.2.1):
// GetFirmwareVersion command: LEN=2, LCS=0xFE, DCS: D4+02=0xD6 -> 0x2A.
var fwCmdFrame = []byte{0x00, 0x00, 0xFF, 0x02, 0xFE, 0xD4, 0x02, 0x2A, 0x00}

// Firmware response D5 03 32 01 06 07: LEN=6, LCS=0xFA,
// DCS: D5+03+32+01+06+07=0x118 -> 0x18 -> 0xE8.
var fwRespFrame = []byte{0x00, 0x00, 0xFF, 0x06, 0xFA, 0xD5, 0x03, 0x32, 0x01, 0x06, 0x07, 0xE8, 0x00}

func TestBuildFrame(t *testing.T) {
	got := buildFrame(pn532TFIOut, []byte{cmdGetFirmwareVersion})
	if !bytes.Equal(got, fwCmdFrame) {
		t.Errorf("firmware command frame = % X, want % X", got, fwCmdFrame)
	}
	// SAMConfiguration: LEN=5, LCS=0xFB, sum D4+14+01+14+01=0xFE -> DCS 0x02.
	wantSAM := []byte{0x00, 0x00, 0xFF, 0x05, 0xFB, 0xD4, 0x14, 0x01, 0x14, 0x01, 0x02, 0x00}
	if got := buildFrame(pn532TFIOut, []byte{cmdSAMConfiguration, 0x01, 0x14, 0x01}); !bytes.Equal(got, wantSAM) {
		t.Errorf("sam command frame = % X, want % X", got, wantSAM)
	}
}

func TestParseFrame(t *testing.T) {
	tfi, data, err := parseFrame(fwRespFrame)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if tfi != pn532TFIIn {
		t.Errorf("tfi = %#02x, want %#02x", tfi, pn532TFIIn)
	}
	if want := []byte{0x03, 0x32, 0x01, 0x06, 0x07}; !bytes.Equal(data, want) {
		t.Errorf("data = % X, want % X", data, want)
	}
	// Trailing padding after the postamble (a fixed-size I2C read) is fine.
	padded := append(append([]byte(nil), fwRespFrame...), 0x00, 0x00, 0x00)
	if _, _, err := parseFrame(padded); err != nil {
		t.Errorf("padded frame rejected: %v", err)
	}
}

func TestParseFrameRejectsCorruption(t *testing.T) {
	badDCS := append([]byte(nil), fwRespFrame...)
	badDCS[len(badDCS)-2] ^= 0xFF
	if _, _, err := parseFrame(badDCS); err == nil {
		t.Error("corrupted data checksum accepted")
	}
	badLCS := append([]byte(nil), fwRespFrame...)
	badLCS[4] ^= 0x01
	if _, _, err := parseFrame(badLCS); err == nil {
		t.Error("corrupted length checksum accepted")
	}
	corruptPayload := append([]byte(nil), fwRespFrame...)
	corruptPayload[6] ^= 0x10 // flip a payload bit, DCS now stale
	if _, _, err := parseFrame(corruptPayload); err == nil {
		t.Error("corrupted payload accepted")
	}
	if _, _, err := parseFrame([]byte{0x01, 0x02, 0x03}); err == nil {
		t.Error("garbage without start code accepted")
	}
	truncated := fwRespFrame[:8]
	if _, _, err := parseFrame(truncated); err == nil {
		t.Error("truncated frame accepted")
	}
}

func TestIsACK(t *testing.T) {
	if !isACK(ackFrame) {
		t.Error("ack frame not recognized")
	}
	if !isACK(ackFrame[1:]) { // without one preamble byte, as read off the wire
		t.Error("ack frame without leading preamble byte not recognized")
	}
	if isACK(fwRespFrame) {
		t.Error("information frame recognized as ack")
	}
	if isACK([]byte{0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00}) {
		t.Error("nack frame recognized as ack")
	}
}

// busOp is one scripted I2C transaction: a write asserts the exact
// bytes the driver must send; a read returns canned bytes (the rest of
// the caller's buffer stays zero, like a padded wire read).
type busOp struct {
	write []byte
	read  []byte
	err   error
}

type scriptBus struct {
	t   *testing.T
	ops []busOp
	i   int
}

func (b *scriptBus) next(kind string) busOp {
	b.t.Helper()
	if b.i >= len(b.ops) {
		b.t.Fatalf("unexpected %s (script exhausted after %d ops)", kind, len(b.ops))
	}
	op := b.ops[b.i]
	b.i++
	return op
}

func (b *scriptBus) Write(p []byte) error {
	b.t.Helper()
	op := b.next("write")
	if op.write == nil {
		b.t.Fatalf("op %d: got write % X, script wants a read", b.i-1, p)
	}
	if !bytes.Equal(p, op.write) {
		b.t.Fatalf("op %d: write % X, want % X", b.i-1, p, op.write)
	}
	return op.err
}

func (b *scriptBus) Read(p []byte) error {
	b.t.Helper()
	op := b.next("read")
	if op.read == nil {
		b.t.Fatalf("op %d: got read, script wants a write", b.i-1)
	}
	copy(p, op.read)
	return op.err
}

func (b *scriptBus) done() {
	b.t.Helper()
	if b.i != len(b.ops) {
		b.t.Fatalf("script not exhausted: %d of %d ops used", b.i, len(b.ops))
	}
}

// ready prefixes canned frame bytes with the I2C ready-status byte.
func ready(frame []byte) []byte { return append([]byte{0x01}, frame...) }

func testPN532(bus pn532Bus) *pn532 {
	p := newPN532(bus)
	p.sleep = func(time.Duration) {}
	return p
}

func TestFirmwareVersionExchange(t *testing.T) {
	bus := &scriptBus{t: t, ops: []busOp{
		{write: fwCmdFrame},
		{read: []byte{0x00}}, // not ready yet: one status poll round
		{read: []byte{0x01}}, // ready
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(fwRespFrame)},
	}}
	ver, rev, err := testPN532(bus).firmwareVersion()
	if err != nil {
		t.Fatalf("firmwareVersion: %v", err)
	}
	if ver != 0x01 || rev != 0x06 {
		t.Errorf("version = %d.%d, want 1.6", ver, rev)
	}
	bus.done()
}

func TestFirmwareVersionRejectsForeignIC(t *testing.T) {
	// Same exchange, but the IC byte is not 0x32: some other device
	// answered - must not be detected as a PN532.
	resp := buildFrame(pn532TFIIn, []byte{0x03, 0x33, 0x01, 0x06, 0x07})
	bus := &scriptBus{t: t, ops: []busOp{
		{write: fwCmdFrame},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(resp)},
	}}
	if _, _, err := testPN532(bus).firmwareVersion(); err == nil {
		t.Error("foreign IC byte accepted as pn532")
	}
	bus.done()
}

func TestSAMConfigurationExchange(t *testing.T) {
	cmd := buildFrame(pn532TFIOut, []byte{cmdSAMConfiguration, 0x01, 0x14, 0x01})
	resp := buildFrame(pn532TFIIn, []byte{0x15})
	bus := &scriptBus{t: t, ops: []busOp{
		{write: cmd},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(resp)},
	}}
	if err := testPN532(bus).samConfiguration(); err != nil {
		t.Fatalf("samConfiguration: %v", err)
	}
	bus.done()
}

func TestSetPassiveRetriesExchange(t *testing.T) {
	cmd := buildFrame(pn532TFIOut, []byte{cmdRFConfiguration, 0x05, 0xFF, 0x01, 0x00})
	resp := buildFrame(pn532TFIIn, []byte{0x33})
	bus := &scriptBus{t: t, ops: []busOp{
		{write: cmd},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(resp)},
	}}
	if err := testPN532(bus).setPassiveRetries(); err != nil {
		t.Fatalf("setPassiveRetries: %v", err)
	}
	bus.done()
}

// inListOps scripts one full InListPassiveTarget exchange returning the
// given response payload (after the D5 4B header, i.e. starting at NbTg).
func inListOps(payload []byte) []busOp {
	cmd := buildFrame(pn532TFIOut, []byte{cmdInListPassiveTarget, 0x01, 0x00})
	resp := buildFrame(pn532TFIIn, append([]byte{0x4B}, payload...))
	return []busOp{
		{write: cmd},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(resp)},
	}
}

func TestInListPassiveTarget(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		uid     []byte
		found   bool
		wantErr bool
	}{
		{name: "no tag", payload: []byte{0x00}, found: false},
		{name: "4-byte uid (mifare classic)",
			payload: []byte{0x01, 0x01, 0x00, 0x04, 0x08, 0x04, 0xD6, 0x45, 0x90, 0x3B},
			uid:     []byte{0xD6, 0x45, 0x90, 0x3B}, found: true},
		{name: "7-byte uid (ntag)",
			payload: []byte{0x01, 0x01, 0x00, 0x44, 0x00, 0x07, 0x04, 0xA3, 0x1B, 0x2C, 0x5D, 0x80, 0x00},
			uid:     []byte{0x04, 0xA3, 0x1B, 0x2C, 0x5D, 0x80, 0x00}, found: true},
		{name: "7-byte uid with trailing ATS (desfire, SAK 0x20)",
			payload: []byte{0x01, 0x01, 0x03, 0x44, 0x20, 0x07, 0x04, 0xA3, 0x1B, 0x2C, 0x5D, 0x80, 0x00,
				0x06, 0x75, 0x77, 0x81, 0x02, 0x80},
			uid: []byte{0x04, 0xA3, 0x1B, 0x2C, 0x5D, 0x80, 0x00}, found: true},
		{name: "implausible uid length",
			payload: []byte{0x01, 0x01, 0x00, 0x04, 0x08, 0x05, 0xD6, 0x45, 0x90, 0x3B, 0x11},
			wantErr: true},
		{name: "truncated uid",
			payload: []byte{0x01, 0x01, 0x00, 0x44, 0x00, 0x07, 0x04, 0xA3},
			wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := &scriptBus{t: t, ops: inListOps(tc.payload)}
			uid, found, err := testPN532(bus).inListPassiveTarget()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("uid = % X found=%v, want error", uid, found)
				}
				return
			}
			if err != nil {
				t.Fatalf("inListPassiveTarget: %v", err)
			}
			if found != tc.found || !bytes.Equal(uid, tc.uid) {
				t.Errorf("uid = % X found=%v, want % X found=%v", uid, found, tc.uid, tc.found)
			}
			bus.done()
		})
	}
}

func TestCallAbortsOnResponseTimeout(t *testing.T) {
	// The command is ACKed but no response ever becomes ready: call must
	// give up within its budget and abort the pending command with a
	// host ACK so the next poll round starts clean.
	ops := []busOp{
		{write: buildFrame(pn532TFIOut, []byte{cmdInListPassiveTarget, 0x01, 0x00})},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
	}
	for i := 0; i < pollReadyAttempts; i++ {
		ops = append(ops, busOp{read: []byte{0x00}})
	}
	ops = append(ops, busOp{write: ackFrame})
	bus := &scriptBus{t: t, ops: ops}
	if _, _, err := testPN532(bus).inListPassiveTarget(); !errors.Is(err, errNotReady) {
		t.Fatalf("err = %v, want errNotReady", err)
	}
	bus.done()
}

func TestCallFailsWithoutACK(t *testing.T) {
	// A response frame where the ACK belongs means the exchange is out
	// of sync: fail - and write the host ACK to abort/flush, so the
	// desync cannot bleed into the next round.
	bus := &scriptBus{t: t, ops: []busOp{
		{write: fwCmdFrame},
		{read: []byte{0x01}},
		{read: ready(fwRespFrame)},
		{write: ackFrame},
	}}
	if _, _, err := testPN532(bus).firmwareVersion(); err == nil {
		t.Error("missing ack accepted")
	}
	bus.done()
}

// TestReadFramePollsThroughReadErrors pins the RPi wake fix: a PN532
// that is still waking NAKs its address, so status reads FAIL instead
// of returning a not-ready byte. Those errors must consume attempts,
// not abort the exchange - aborting made a healthy reader silently
// classify as "no hardware".
func TestReadFramePollsThroughReadErrors(t *testing.T) {
	nak := errors.New("remote i/o error")
	bus := &scriptBus{t: t, ops: []busOp{
		{write: fwCmdFrame},
		{read: []byte{0x00}, err: nak}, // NAK while waking
		{read: []byte{0x00}, err: nak}, // still waking
		{read: []byte{0x00}},           // awake, frame not ready yet
		{read: []byte{0x01}},           // ready
		{read: ready(ackFrame)},
		{read: []byte{0x00}, err: nak}, // response wait hits one more NAK
		{read: []byte{0x01}},
		{read: ready(fwRespFrame)},
	}}
	ver, rev, err := testPN532(bus).firmwareVersion()
	if err != nil {
		t.Fatalf("firmwareVersion through NAKs: %v", err)
	}
	if ver != 0x01 || rev != 0x06 {
		t.Errorf("version = %d.%d, want 1.6", ver, rev)
	}
	bus.done()
}

// TestReadFrameReportsLastReadError: when the budget runs out on a
// persistently NAKing chip, the error must stay errNotReady (for the
// callers' checks) and carry the underlying read error (for the per-bus
// probe log).
func TestReadFrameReportsLastReadError(t *testing.T) {
	nak := errors.New("remote i/o error")
	ops := []busOp{{write: fwCmdFrame}}
	for i := 0; i < ackReadyAttempts; i++ {
		ops = append(ops, busOp{read: []byte{0x00}, err: nak})
	}
	ops = append(ops, busOp{write: ackFrame}) // abort/resync still happens
	bus := &scriptBus{t: t, ops: ops}
	_, _, err := testPN532(bus).firmwareVersion()
	if !errors.Is(err, errNotReady) {
		t.Fatalf("err = %v, want errNotReady", err)
	}
	if !strings.Contains(err.Error(), "remote i/o error") {
		t.Errorf("underlying read error not reported: %v", err)
	}
	bus.done()
}

// TestReadFrameDeadlineBoundsSlowErrors: attempt counting bounds fast
// NAKs, but an errored read that itself blocks (wedged bus, endless
// clock stretch, ~1s kernel i2c timeout each) must hit the wall-clock
// deadline instead of stalling for attempts x timeout.
func TestReadFrameDeadlineBoundsSlowErrors(t *testing.T) {
	bus := &scriptBus{t: t, ops: []busOp{
		{write: fwCmdFrame},
		{read: []byte{0x00}, err: errors.New("i2c transfer timed out")},
		{write: ackFrame}, // abort/resync after the deadline fires
	}}
	p := testPN532(bus)
	base := time.Unix(0, 0)
	calls := 0
	p.now = func() time.Time {
		calls++
		// Each call advances far past the deadline: the first computes
		// it, the second (before attempt 2) must trip it.
		return base.Add(time.Duration(calls) * time.Minute)
	}
	_, _, err := p.firmwareVersion()
	if !errors.Is(err, errNotReady) {
		t.Fatalf("err = %v, want errNotReady", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("underlying slow error not reported: %v", err)
	}
	bus.done()
}

// probeChip wake-sequence tests: the field-facing detection order (SAM
// wake first, firmware decides, one delayed full retry) - pinned
// without hardware, exactly the sequence that silently failed on the
// RPi before the wake fix.
func samOKOps() []busOp {
	cmd := buildFrame(pn532TFIOut, []byte{cmdSAMConfiguration, 0x01, 0x14, 0x01})
	resp := buildFrame(pn532TFIIn, []byte{0x15})
	return []busOp{
		{write: cmd},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(resp)},
	}
}

func fwOKOps() []busOp {
	return []busOp{
		{write: fwCmdFrame},
		{read: []byte{0x01}},
		{read: ready(ackFrame)},
		{read: []byte{0x01}},
		{read: ready(fwRespFrame)},
	}
}

func TestProbeChipWakesSleepingReader(t *testing.T) {
	// First round: the chip NAKs everything (LowVbat / oscillator
	// start) - both writes fail. After the wake delay the retry round
	// succeeds end-to-end.
	nak := errors.New("remote i/o error")
	samCmd := buildFrame(pn532TFIOut, []byte{cmdSAMConfiguration, 0x01, 0x14, 0x01})
	var ops []busOp
	ops = append(ops, busOp{write: samCmd, err: nak})
	ops = append(ops, busOp{write: fwCmdFrame, err: nak})
	ops = append(ops, samOKOps()...)
	ops = append(ops, fwOKOps()...)
	bus := &scriptBus{t: t, ops: ops}
	slept := 0
	ver, rev, err := probeChip(testPN532(bus), func(d time.Duration) {
		slept++
		if d != wakeRetryDelay {
			t.Errorf("retry slept %v, want %v", d, wakeRetryDelay)
		}
	})
	if err != nil {
		t.Fatalf("probeChip: %v", err)
	}
	if ver != 0x01 || rev != 0x06 || slept != 1 {
		t.Errorf("ver=%d rev=%d slept=%d, want 1 6 1", ver, rev, slept)
	}
	bus.done()
}

func TestProbeChipLeavesForeignDeviceAlone(t *testing.T) {
	// A device that answers the firmware query coherently but with a
	// foreign IC byte must yield errNotPN532 WITHOUT the wake retry -
	// the exhausted script proves no second round was attempted.
	foreign := buildFrame(pn532TFIIn, []byte{0x03, 0x33, 0x01, 0x06, 0x07})
	var ops []busOp
	ops = append(ops, samOKOps()...)
	ops = append(ops,
		busOp{write: fwCmdFrame},
		busOp{read: []byte{0x01}},
		busOp{read: ready(ackFrame)},
		busOp{read: []byte{0x01}},
		busOp{read: ready(foreign)},
	)
	bus := &scriptBus{t: t, ops: ops}
	_, _, err := probeChip(testPN532(bus), func(time.Duration) { t.Error("foreign device retried") })
	if !errors.Is(err, errNotPN532) {
		t.Fatalf("err = %v, want errNotPN532", err)
	}
	bus.done()
}

func TestCallAbortsOnACKTimeout(t *testing.T) {
	// The chip never signals readiness for the ACK: same abort/resync
	// before giving up.
	ops := []busOp{{write: fwCmdFrame}}
	for i := 0; i < ackReadyAttempts; i++ {
		ops = append(ops, busOp{read: []byte{0x00}})
	}
	ops = append(ops, busOp{write: ackFrame})
	bus := &scriptBus{t: t, ops: ops}
	if _, _, err := testPN532(bus).firmwareVersion(); !errors.Is(err, errNotReady) {
		t.Fatalf("err = %v, want errNotReady", err)
	}
	bus.done()
}
