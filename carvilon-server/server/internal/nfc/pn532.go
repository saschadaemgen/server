package nfc

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// This file is the first-party PN532 protocol layer (NXP UM0701-02) over
// an abstract I2C transaction bus - no libnfc, no CGO. It speaks the
// normal-frame format (preamble + start code, LEN/LCS, TFI, DCS) and the
// minimal command set this track needs: GetFirmwareVersion (the probe),
// SAMConfiguration (normal mode, doubling as the wake-up command),
// RFConfiguration MaxRetries (prompt "no tag" returns) and
// InListPassiveTarget (ISO14443A tag poll with UID parsing). It lives
// untagged so the frame math and command exchanges are unit-tested
// against recorded byte sequences on any platform; only the real
// /dev/i2c-* bus behind pn532Bus is Linux-only.

// pn532Bus is one I2C peer endpoint: every Write/Read is a single I2C
// transaction against the PN532's address. The Linux implementation
// sits on /dev/i2c-*; tests script the transactions.
type pn532Bus interface {
	Write(p []byte) error
	Read(p []byte) error
}

// PN532 frame constants (UM0701-02 §6.2.1).
const (
	pn532TFIOut = 0xD4 // frame identifier host -> PN532
	pn532TFIIn  = 0xD5 // frame identifier PN532 -> host
	pn532IC     = 0x32 // GetFirmwareVersion IC byte identifying a PN532
)

// Command codes (a response carries code+1).
const (
	cmdGetFirmwareVersion  = 0x02
	cmdSAMConfiguration    = 0x14
	cmdRFConfiguration     = 0x32
	cmdInListPassiveTarget = 0x4A
)

// Readiness budgets. The PN532 prefixes every I2C read with a status
// byte (bit 0 = a frame is ready); we poll it in readyInterval steps up
// to a per-exchange attempt budget. ACKs and register-style commands
// answer within a few milliseconds; InListPassiveTarget runs a full RF
// activation sequence (field on, guard time, anticollision), so it gets
// a far larger budget - still comfortably below the 250 ms poll cycle.
const (
	readyInterval     = 2 * time.Millisecond
	ackReadyAttempts  = 10  // ~20 ms
	cmdReadyAttempts  = 40  // ~80 ms (firmware / SAM / RFConfiguration)
	pollReadyAttempts = 100 // ~200 ms (InListPassiveTarget RF sequence)
)

// respBufSize is the single-transaction response read. Large enough for
// every response in scope (the worst case, a 10-byte UID plus a maximal
// ATS, stays under 50 bytes on the wire); the PN532 pads extra clocked
// bytes, which the frame's LEN simply ignores.
const respBufSize = 64

// ackFrame is the fixed ACK frame (§6.2.1.3): sent by the chip after
// every valid command, and by the host to abort a pending command.
var ackFrame = []byte{0x00, 0x00, 0xFF, 0x00, 0xFF, 0x00}

var errNotReady = errors.New("nfc: pn532 not ready in time")

// pn532 drives one chip over a transaction bus. sleep is injectable so
// the exchange logic tests clock-free.
type pn532 struct {
	bus   pn532Bus
	sleep func(time.Duration)
}

func newPN532(bus pn532Bus) *pn532 { return &pn532{bus: bus, sleep: time.Sleep} }

// buildFrame wraps TFI + data in a normal information frame:
// 00 00 FF LEN LCS <TFI data...> DCS 00, with LEN counting TFI+data,
// LEN+LCS == 0 and TFI+sum(data)+DCS == 0 (mod 256).
func buildFrame(tfi byte, data []byte) []byte {
	ln := len(data) + 1
	sum := tfi
	for _, b := range data {
		sum += b
	}
	f := make([]byte, 0, ln+7)
	f = append(f, 0x00, 0x00, 0xFF, byte(ln), byte(0x100-ln), tfi)
	f = append(f, data...)
	f = append(f, -sum, 0x00)
	return f
}

// parseFrame extracts TFI + payload from a raw read buffer: it scans for
// the 00 FF start code (tolerating leading preamble/idle bytes), then
// validates LEN/LCS and the DCS data checksum. A corrupted frame is
// rejected whole, never partially trusted.
func parseFrame(buf []byte) (tfi byte, data []byte, err error) {
	i := 0
	for ; i+1 < len(buf); i++ {
		if buf[i] == 0x00 && buf[i+1] == 0xFF {
			break
		}
	}
	if i+1 >= len(buf) {
		return 0, nil, errors.New("nfc: no frame start code")
	}
	rest := buf[i+2:]
	if len(rest) < 2 {
		return 0, nil, errors.New("nfc: truncated frame header")
	}
	ln := int(rest[0])
	if rest[0]+rest[1] != 0 {
		return 0, nil, errors.New("nfc: length checksum mismatch")
	}
	if ln == 0 {
		return 0, nil, errors.New("nfc: empty frame")
	}
	if len(rest) < 2+ln+1 {
		return 0, nil, errors.New("nfc: truncated frame body")
	}
	body := rest[2 : 2+ln]
	var sum byte
	for _, b := range body {
		sum += b
	}
	if sum+rest[2+ln] != 0 {
		return 0, nil, errors.New("nfc: data checksum mismatch")
	}
	return body[0], append([]byte(nil), body[1:]...), nil
}

// isACK reports the ACK frame (start code followed by 00 FF, i.e. the
// LEN=0 special frame), tolerating the same leading bytes as parseFrame.
func isACK(buf []byte) bool {
	for i := 0; i+3 < len(buf); i++ {
		if buf[i] == 0x00 && buf[i+1] == 0xFF {
			return buf[i+2] == 0x00 && buf[i+3] == 0xFF
		}
	}
	return false
}

// readFrame polls the I2C status byte until the chip flags a pending
// frame, then reads it in one transaction. Every read transaction
// restarts at a fresh status byte - the PN532 replays its output per
// transaction - so the returned slice has the status byte stripped.
func (p *pn532) readFrame(attempts, size int) ([]byte, error) {
	st := make([]byte, 1)
	for i := 0; i < attempts; i++ {
		if err := p.bus.Read(st); err != nil {
			return nil, err
		}
		if st[0]&0x01 == 1 {
			buf := make([]byte, size+1)
			if err := p.bus.Read(buf); err != nil {
				return nil, err
			}
			return buf[1:], nil
		}
		p.sleep(readyInterval)
	}
	return nil, errNotReady
}

// call sends one command and returns the response payload after the
// response-code byte. It performs the full §6.2 I2C exchange: write the
// command frame, await the ACK, await the response frame within the
// command's budget. Every failed exchange writes a host ACK (§6.2.1.5)
// before returning: it aborts a still-pending command and flushes a
// desynced output buffer, so one bad round cannot bleed stale frames
// into the next poll round.
func (p *pn532) call(cmd byte, args []byte, respAttempts int) ([]byte, error) {
	frame := buildFrame(pn532TFIOut, append([]byte{cmd}, args...))
	if err := p.bus.Write(frame); err != nil {
		return nil, fmt.Errorf("nfc: write command %#02x: %w", cmd, err)
	}
	ack, err := p.readFrame(ackReadyAttempts, len(ackFrame))
	if err != nil {
		_ = p.bus.Write(ackFrame) // abort/resync, best effort
		return nil, fmt.Errorf("nfc: await ack for command %#02x: %w", cmd, err)
	}
	if !isACK(ack) {
		_ = p.bus.Write(ackFrame) // abort/resync, best effort
		return nil, fmt.Errorf("nfc: no ack for command %#02x", cmd)
	}
	resp, err := p.readFrame(respAttempts, respBufSize)
	if err != nil {
		_ = p.bus.Write(ackFrame) // abort the pending command
		return nil, fmt.Errorf("nfc: await response for command %#02x: %w", cmd, err)
	}
	tfi, data, err := parseFrame(resp)
	if err != nil {
		return nil, err
	}
	if tfi != pn532TFIIn || len(data) == 0 || data[0] != cmd+1 {
		return nil, fmt.Errorf("nfc: unexpected response to command %#02x", cmd)
	}
	return data[1:], nil
}

// errNotPN532 marks a coherent firmware answer from something that is
// not a PN532 - the probe must not keep poking such a device.
var errNotPN532 = errors.New("nfc: not a pn532")

// firmwareVersion probes the chip (§7.2.2). The IC byte must be 0x32:
// that is what makes a firmware answer proof of a PN532 (a stray device
// ACKing the address cannot produce a valid frame with this IC).
func (p *pn532) firmwareVersion() (ver, rev byte, err error) {
	data, err := p.call(cmdGetFirmwareVersion, nil, cmdReadyAttempts)
	if err != nil {
		return 0, 0, err
	}
	if len(data) < 4 || data[0] != pn532IC {
		return 0, 0, fmt.Errorf("%w (firmware response % X)", errNotPN532, data)
	}
	return data[1], data[2], nil
}

// samConfiguration puts the chip in normal mode (§7.2.10): mode 0x01;
// the timeout byte matters only in virtual-card mode; IRQ handling on
// is harmless since we poll the status byte. It doubles as the wake-up
// command - after power-on in the LowVbat condition the PN532 processes
// nothing before SAMConfiguration.
func (p *pn532) samConfiguration() error {
	_, err := p.call(cmdSAMConfiguration, []byte{0x01, 0x14, 0x01}, cmdReadyAttempts)
	return err
}

// setPassiveRetries caps InListPassiveTarget at a single activation
// attempt (§7.3.1 CfgItem 0x05) so a poll with no tag in the field
// returns "no target" promptly instead of retrying forever (0xFF, the
// power-up default, blocks until a tag arrives).
func (p *pn532) setPassiveRetries() error {
	// MxRtyATR 0xFF (default), MxRtyPSL 0x01 (default),
	// MxRtyPassiveActivation 0x00 (single attempt, no retry).
	_, err := p.call(cmdRFConfiguration, []byte{0x05, 0xFF, 0x01, 0x00}, cmdReadyAttempts)
	return err
}

// inListPassiveTarget polls for one ISO14443A target at 106 kbps
// (§7.3.5) and returns its UID; found is false when no tag is in the
// field. The target record is [Tg SENS_RES(2) SEL_RES NFCIDLength
// NFCID1...]; the chip resolves the cascade levels itself, so the UID
// arrives complete (4, 7 or 10 bytes). An ISO14443-4 tag (DESFire,
// smartphone) appends its ATS after the UID - ignored here, the frame
// checksum already covered it.
func (p *pn532) inListPassiveTarget() (uid []byte, found bool, err error) {
	// MaxTg 0x01 (one target), BrTy 0x00 (106 kbps ISO14443 type A).
	data, err := p.call(cmdInListPassiveTarget, []byte{0x01, 0x00}, pollReadyAttempts)
	if err != nil {
		return nil, false, err
	}
	if len(data) < 1 {
		return nil, false, errors.New("nfc: empty target response")
	}
	if data[0] == 0 {
		return nil, false, nil
	}
	if len(data) < 6 {
		return nil, false, errors.New("nfc: truncated target record")
	}
	n := int(data[5])
	if n != 4 && n != 7 && n != 10 {
		return nil, false, fmt.Errorf("nfc: implausible uid length %d", n)
	}
	if len(data) < 6+n {
		return nil, false, errors.New("nfc: truncated uid")
	}
	return append([]byte(nil), data[6:6+n]...), true, nil
}

// pn532Reader adapts one configured PN532 to the reader-model seam.
type pn532Reader struct {
	p *pn532
	c io.Closer // the underlying bus
}

func (r *pn532Reader) Poll() ([]byte, bool, error) { return r.p.inListPassiveTarget() }
func (r *pn532Reader) Close() error                { return r.c.Close() }
