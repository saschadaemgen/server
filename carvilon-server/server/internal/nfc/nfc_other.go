//go:build !linux

package nfc

// platformProbe has nothing to offer off Linux: the PN532 sits on
// /dev/i2c-*, so no reader ever surfaces on the dev machine
// (Windows/macOS) or any other non-Linux host, and the NFC category
// stays absent from the editor palette.
func platformProbe() (Status, []detectedReader) { return Unavailable, nil }
