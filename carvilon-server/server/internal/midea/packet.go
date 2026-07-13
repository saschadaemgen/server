package midea

import (
	"encoding/binary"
	"errors"
	"time"
)

// timestamp8 baut den 8-Byte-Zeitstempel des 5A5A-Headers.
// Reihenfolge: [µs/10000, Sekunde, Minute, Stunde, Tag, Monat, Jahr%100, Jahr/100].
func timestamp8(now time.Time) []byte {
	return []byte{
		byte(now.Nanosecond() / 10_000_000), // Zehntel-Millisekunden (µs/10000)
		byte(now.Second()),
		byte(now.Minute()),
		byte(now.Hour()),
		byte(now.Day()),
		byte(int(now.Month())),
		byte(now.Year() % 100),
		byte(now.Year() / 100),
	}
}

// encodePacket verpackt einen 0xAA-Command-Frame in ein 5A5A-Paket.
// deviceID ist die numerische Geräte-ID aus der Discovery.
func encodePacket(deviceID uint64, command []byte) ([]byte, error) {
	enc, err := encryptAESECB(command)
	if err != nil {
		return nil, err
	}

	length := 40 + len(enc) + 16

	header := make([]byte, 40)
	header[0], header[1] = 0x5A, 0x5A // Start of packet
	header[2], header[3] = 0x01, 0x11 // Message type
	binary.LittleEndian.PutUint16(header[4:6], uint16(length))
	header[6], header[7] = 0x20, 0x00 // Magic bytes
	// header[8:12] Message ID = 0
	copy(header[12:20], timestamp8(time.Now().UTC())) // Timestamp
	binary.LittleEndian.PutUint64(header[20:28], deviceID)
	// header[28:40] = 12 Nullbytes

	packet := append(header, enc...)
	packet = append(packet, sign(packet)...) // 16-Byte-MD5
	return packet, nil
}

// decodePacket entpackt ein empfangenes 5A5A-Paket zum Command-Frame.
func decodePacket(data []byte) ([]byte, error) {
	if len(data) < 6 {
		return nil, errors.New("midea: 5A5A-Paket zu kurz")
	}
	if data[0] != 0x5A || data[1] != 0x5A {
		return nil, errors.New("midea: kein 5A5A-Startmarker")
	}
	length := int(binary.LittleEndian.Uint16(data[4:6]))
	if len(data) < length {
		return nil, errors.New("midea: 5A5A-Paket abgeschnitten")
	}
	packet := data[:length]
	encFrame := packet[40 : len(packet)-16]
	rxHash := packet[len(packet)-16:]

	if !equalBytes(sign(packet[:len(packet)-16]), rxHash) {
		return nil, errors.New("midea: MD5-Signatur stimmt nicht")
	}
	return decryptAESECB(encFrame)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
