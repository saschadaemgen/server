//go:build linux

package nfc

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Real PN532 access: raw /dev/i2c-* transactions with the I2C_SLAVE
// ioctl - standard library only (stdlib syscall, matching sysmetrics'
// statfs), no CGO, no third-party bus stack.

const (
	// i2cSlave is I2C_SLAVE from <linux/i2c-dev.h> (not exported by the
	// stdlib or x/sys): select the peer address for the following
	// read/write transactions on the fd.
	i2cSlave = 0x0703
	// pn532I2CAddr is the PN532's fixed 7-bit I2C address (0x48 in the
	// manual's 8-bit form; the Elechouse V3 module has no address strap).
	pn532I2CAddr = 0x24
)

// i2cDev is one open I2C peer: every Read/Write is a single I2C
// transaction against the selected address.
type i2cDev struct{ f *os.File }

func (d *i2cDev) Write(p []byte) error { _, err := d.f.Write(p); return err }
func (d *i2cDev) Read(p []byte) error  { _, err := d.f.Read(p); return err }
func (d *i2cDev) Close() error         { return d.f.Close() }

// openBus opens an I2C bus device and selects the PN532 address. An
// EACCES surfaces as fs.ErrPermission (via *PathError) for classify's
// Forbidden outcome.
func openBus(dev string) (*i2cDev, error) {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), i2cSlave, uintptr(pn532I2CAddr)); errno != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("nfc: select address %#02x on %s: %w", pn532I2CAddr, dev, errno)
	}
	return &i2cDev{f: f}, nil
}

// platformProbe scans the I2C buses for PN532 readers. A firmware
// answer carrying the PN532 IC byte is the proof of a reader; buses
// without one (or with unrelated devices) stay silent.
func platformProbe() (Status, []detectedReader) {
	devs, _ := filepath.Glob("/dev/i2c-*")
	return classify(devs, probeBus, currentLogger())
}

// probeBus checks one bus for a PN532 and, on success, returns the
// reader plus an opener that reopens and configures the device for a
// run. The detection exchange itself (SAM wake first, firmware answer
// decides, one delayed retry) lives in probeChip. Tradeoff this probe
// cannot avoid: an addressed-slave query IS a bus write, so a foreign
// device parked at 0x24 sees a few command bytes at startup. Cost per
// bus: an empty address NAKs the writes immediately (~50 ms, the retry
// delay dominates); the absolute worst case - a write-ACKing but mute
// device - stays around half a second. Probe runs once, sequentially,
// on the startup path.
func probeBus(dev string) (detectedReader, error) {
	bus, err := openBus(dev)
	if err != nil {
		return detectedReader{}, err
	}
	defer bus.Close()
	p := newPN532(bus)
	ver, rev, err := probeChip(p, time.Sleep)
	if err != nil {
		return detectedReader{}, err
	}
	id := filepath.Base(dev)
	return detectedReader{
		info: ReaderInfo{ID: id, Model: "PN532", Firmware: fmt.Sprintf("%d.%d", ver, rev)},
		open: func() (tagReader, error) { return openReader(dev) },
	}, nil
}

// openReader opens and configures one PN532 for a run: normal mode plus
// single-attempt passive activation, so every poll round returns
// promptly whether or not a tag is in the field.
func openReader(dev string) (tagReader, error) {
	bus, err := openBus(dev)
	if err != nil {
		return nil, err
	}
	p := newPN532(bus)
	if err := p.samConfiguration(); err != nil {
		// One retry: the chip may still be waking (see probeBus).
		time.Sleep(wakeRetryDelay)
		if err = p.samConfiguration(); err != nil {
			_ = bus.Close()
			return nil, err
		}
	}
	if err := p.setPassiveRetries(); err != nil {
		_ = bus.Close()
		return nil, err
	}
	return &pn532Reader{p: p, c: bus}, nil
}
