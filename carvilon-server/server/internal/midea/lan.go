package midea

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// 8370-Pakettypen (unteres Nibble des Typ-Bytes).
const (
	pktHandshakeRequest  = 0x0
	pktHandshakeResponse = 0x1
	pktEncryptedResponse = 0x3
	pktEncryptedRequest  = 0x6
	pktError             = 0xF
)

// conn kapselt die TCP-Verbindung und den nach dem Handshake gültigen Session-Key.
type conn struct {
	tcp      net.Conn
	localKey []byte // 32 Byte, erst nach authenticate gesetzt
	packetID uint16 // 12-Bit-Zähler
}

// dial öffnet eine TCP-Verbindung zu ip:port (Standard 6444).
func dial(ip string, port int, timeout time.Duration) (*conn, error) {
	tcp, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), timeout)
	if err != nil {
		return nil, fmt.Errorf("midea: Verbindung fehlgeschlagen: %w", err)
	}
	return &conn{tcp: tcp}, nil
}

func (c *conn) close() error { return c.tcp.Close() }

// buildHeader baut den 6-Byte-8370-Header.
func buildHeader(length int, extra byte) []byte {
	h := make([]byte, 6)
	h[0], h[1] = 0x83, 0x70
	binary.BigEndian.PutUint16(h[2:4], uint16(length))
	h[4] = 0x20
	h[5] = extra
	return h
}

// nextID liefert die nächste 12-Bit-Paket-ID.
func (c *conn) nextID() uint16 {
	id := c.packetID
	c.packetID = (c.packetID + 1) & 0xFFF
	return id
}

// encodeHandshakeRequest verpackt den Token für den Handshake.
func (c *conn) encodeHandshakeRequest(token []byte) []byte {
	header := buildHeader(len(token), pktHandshakeRequest)
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, c.nextID())
	payload = append(payload, token...)
	return append(header, payload...)
}

// encodeEncryptedRequest verschlüsselt und signiert ein 5A5A-Paket für den Versand.
func (c *conn) encodeEncryptedRequest(data []byte) ([]byte, error) {
	if c.localKey == nil {
		return nil, errors.New("midea: nicht authentifiziert")
	}
	// Padding auf 16-Byte-Ausrichtung, inkl. 2-Byte-Paket-ID.
	remainder := (len(data) + 2) % 16
	pad := 0
	if remainder != 0 {
		pad = 16 - remainder
	}
	length := len(data) + pad + 32

	header := buildHeader(length, byte(pad<<4|pktEncryptedRequest))

	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, c.nextID())
	payload = append(payload, data...)
	if pad > 0 {
		padBytes := make([]byte, pad)
		_, _ = rand.Read(padBytes)
		payload = append(payload, padBytes...)
	}

	h := sha256Sum(header, payload)
	encPayload, err := encryptAESCBC(c.localKey, payload)
	if err != nil {
		return nil, err
	}
	out := append(append([]byte{}, header...), encPayload...)
	return append(out, h...), nil
}

// readPacket liest ein vollständiges 8370-Paket vom Socket.
func (c *conn) readPacket(timeout time.Duration) ([]byte, error) {
	_ = c.tcp.SetReadDeadline(time.Now().Add(timeout))

	head := make([]byte, 6)
	if _, err := io.ReadFull(c.tcp, head); err != nil {
		return nil, err
	}
	if head[0] != 0x83 || head[1] != 0x70 {
		return nil, fmt.Errorf("midea: ungültiger 8370-Start: %x", head[:2])
	}
	// total = 6 Header + 2 Paket-ID + Nutzlast(+Sign). size-Feld deckt Payload+Sign
	// (inkl. der 2 ID-Bytes fürs Padding), daher +8 abzüglich des bereits gelesenen Headers.
	total := int(binary.BigEndian.Uint16(head[2:4])) + 8
	rest := make([]byte, total-6)
	if _, err := io.ReadFull(c.tcp, rest); err != nil {
		return nil, err
	}
	return append(head, rest...), nil
}

// processPacket dekodiert ein empfangenes 8370-Paket zur Klartext-Nutzlast.
func (c *conn) processPacket(packet []byte) ([]byte, error) {
	if packet[4] != 0x20 {
		return nil, fmt.Errorf("midea: ungültiges Magic-Byte 0x%X", packet[4])
	}
	switch packet[5] & 0xF {
	case pktEncryptedResponse:
		return c.decodeEncryptedResponse(packet)
	case pktHandshakeResponse:
		return packet[8:], nil // 6 Header + 2 Paket-ID abschneiden
	case pktError:
		return nil, errors.New("midea: Fehlerpaket empfangen")
	default:
		return nil, fmt.Errorf("midea: unerwarteter Pakettyp %d", packet[5]&0xF)
	}
}

// decodeEncryptedResponse entschlüsselt eine verschlüsselte Antwort.
func (c *conn) decodeEncryptedResponse(packet []byte) ([]byte, error) {
	header := packet[:6]
	payload := packet[6 : len(packet)-32]
	rxHash := packet[len(packet)-32:]

	dec, err := decryptAESCBC(c.localKey, payload)
	if err != nil {
		return nil, err
	}
	if !equalBytes(sha256Sum(header, dec), rxHash) {
		return nil, errors.New("midea: SHA256 der Antwort stimmt nicht")
	}
	pad := int(header[5] >> 4)
	if pad == 0 {
		return dec[2:], nil
	}
	return dec[2 : len(dec)-pad], nil
}

// authenticate führt den V3-Handshake durch und leitet den Session-Key ab.
// token (64 Byte) und key (32 Byte) stammen aus der Discovery/Cloud.
func (c *conn) authenticate(token, key []byte, timeout time.Duration) error {
	if len(token) == 0 || len(key) == 0 {
		return errors.New("midea: Token und Key erforderlich")
	}
	if _, err := c.tcp.Write(c.encodeHandshakeRequest(token)); err != nil {
		return err
	}
	packet, err := c.readPacket(timeout)
	if err != nil {
		return fmt.Errorf("midea: Handshake-Antwort: %w", err)
	}
	resp, err := c.processPacket(packet)
	if err != nil {
		return err
	}
	if len(resp) != 64 {
		return fmt.Errorf("midea: unerwartete Handshake-Länge %d", len(resp))
	}
	// resp = 32 Byte verschlüsselte Nutzlast + 32 Byte SHA256.
	dec, err := decryptAESCBC(key, resp[:32])
	if err != nil {
		return err
	}
	if !equalBytes(sha256Sum(dec), resp[32:]) {
		return errors.New("midea: Handshake-SHA256 stimmt nicht")
	}
	c.localKey = xorBytes(dec, key) // Session-Key = decrypted XOR key
	return nil
}

// drainUnsolicited liest unaufgeforderte Frames (z. B. den Status-Push direkt
// nach dem Login) und gibt sie zurück, damit nachfolgende Abfragen eine saubere
// Leitung haben.
func (c *conn) drainUnsolicited(timeout time.Duration) [][]byte {
	var frames [][]byte
	for i := 0; i < 4; i++ {
		f, err := c.readResponseFrame(timeout)
		if err != nil {
			break
		}
		frames = append(frames, f)
	}
	return frames
}

// readResponseFrame liest, entschlüsselt und dekodiert einen Antwort-Frame.
func (c *conn) readResponseFrame(timeout time.Duration) ([]byte, error) {
	packet, err := c.readPacket(timeout)
	if err != nil {
		return nil, err
	}
	plain, err := c.processPacket(packet)
	if err != nil {
		return nil, err
	}
	return decodePacket(plain)
}

// sendCommand sendet einen 0xAA-Frame (als 5A5A verpackt) und liefert alle
// dekodierten Antwort-Frames zurück. Nach dem ersten Frame werden weitere mit
// kurzem Timeout eingesammelt (manche Geräte antworten mehrteilig, z. B. bei
// Fähigkeits-Abfragen).
func (c *conn) sendCommand(deviceID uint64, frame []byte, timeout time.Duration) ([][]byte, error) {
	inner, err := encodePacket(deviceID, frame)
	if err != nil {
		return nil, err
	}
	req, err := c.encodeEncryptedRequest(inner)
	if err != nil {
		return nil, err
	}
	if _, err := c.tcp.Write(req); err != nil {
		return nil, err
	}

	first, err := c.readResponseFrame(timeout)
	if err != nil {
		return nil, err
	}
	frames := [][]byte{first}

	// Weitere Frames ohne langes Blockieren nachlesen.
	for i := 0; i < 4; i++ {
		f, ferr := c.readResponseFrame(500 * time.Millisecond)
		if ferr != nil {
			break
		}
		frames = append(frames, f)
	}
	return frames, nil
}
