// mqtt-spy is a passive MQTT listener used in saison 11 to learn
// which RPCs UDM publishes outside the mock's own device topic.
// It connects to the UDM broker with a per-mock state bundle
// (broker certs + CA from a completed adoption), tries a sequence
// of subscribe patterns until UDM accepts one, and dumps every
// frame as hex+ASCII to a log file.
//
// Spy is strictly passive: no heartbeat, no RPC responses, no
// state modifications. Disconnect cleanly on SIGINT/SIGTERM.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"unifix.local/mock/internal/state"
	"unifix.local/shared/proto"
)

const (
	connectTimeout   = 15 * time.Second
	subscribeTimeout = 5 * time.Second
	hexBytesPerLine  = 32
)

func main() {
	bundleFromFlag := flag.String("bundle-from", defaultBundlePath(),
		"path to per-mock state dir (must contain bundle.json and certs/)")
	logFlag := flag.String("log", "/tmp/mqtt-spy.log", "log file path")
	connectAsFlag := flag.String("connect-as", "",
		"MQTT client id; default is the bundle dir basename")
	flag.Parse()

	logFile, err := os.OpenFile(*logFlag, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("spy: open log %s: %v", *logFlag, err)
	}
	defer logFile.Close()
	out := log.New(io.MultiWriter(os.Stdout, logFile), "", log.LstdFlags)
	out.Printf("spy: starting, bundle=%s log=%s", *bundleFromFlag, *logFlag)

	baseDir := filepath.Dir(*bundleFromFlag)
	mockID := filepath.Base(*bundleFromFlag)
	store, err := state.New(baseDir)
	if err != nil {
		out.Fatalf("spy: state.New %s: %v", baseDir, err)
	}
	bundle, err := store.LoadBundle(mockID)
	if err != nil {
		out.Fatalf("spy: load bundle for %s: %v", mockID, err)
	}
	if bundle == nil {
		out.Fatalf("spy: no bundle.json under %s", *bundleFromFlag)
	}
	if bundle.BrokerAddress == "" || bundle.ControllerID == "" {
		out.Fatalf("spy: bundle incomplete: broker=%q controller_id=%q",
			bundle.BrokerAddress, bundle.ControllerID)
	}

	clientID := *connectAsFlag
	if clientID == "" {
		clientID = mockID
	}

	tlsConfig, err := buildTLSConfig(store.CertDir(mockID))
	if err != nil {
		out.Fatalf("spy: tls: %v", err)
	}

	brokerURL := strings.Replace(bundle.BrokerAddress, "tls://", "ssl://", 1)

	opts := paho.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(clientID)
	opts.SetTLSConfig(tlsConfig)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(connectTimeout)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetOnConnectHandler(func(_ paho.Client) {
		out.Printf("spy: connected to %s as client_id=%s", brokerURL, clientID)
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		out.Printf("spy: connection lost: %v", err)
	})

	client := paho.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(connectTimeout) {
		out.Fatalf("spy: connect timeout")
	}
	if err := token.Error(); err != nil {
		out.Fatalf("spy: connect: %v", err)
	}
	defer client.Disconnect(500)

	handler := newMessageHandler(out)
	strategies := []string{
		"/uctrl/#",
		fmt.Sprintf("/uctrl/%s/#", bundle.ControllerID),
		fmt.Sprintf("/uctrl/%s/device/+/rpc/+/+", bundle.ControllerID),
	}
	var subscribed string
	for _, topic := range strategies {
		st := client.Subscribe(topic, 0, handler)
		if !st.WaitTimeout(subscribeTimeout) {
			out.Printf("spy: subscribe to %s timed out", topic)
			continue
		}
		if err := st.Error(); err != nil {
			out.Printf("spy: subscribe to %s failed: %v", topic, err)
			continue
		}
		out.Printf("spy: subscribed to %s", topic)
		subscribed = topic
		break
	}
	if subscribed == "" {
		out.Printf("spy: all subscribe strategies failed, exiting")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	out.Printf("spy: shutdown requested, disconnecting")
}

func defaultBundlePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/home/sash710/carvilon-server/state/0cea14424242"
	}
	return filepath.Join(home, "carvilon-server", "state", "0cea14424242")
}

// buildTLSConfig replicates the saison-11 mock mTLS setup so the
// spy can connect with the same cert+CA pinning. Kept local to
// the spy on purpose so the mock package needs no exports.
func buildTLSConfig(certDir string) (*tls.Config, error) {
	crtPath := filepath.Join(certDir, "broker.crt")
	keyPath := filepath.Join(certDir, "broker.key")
	caPath := filepath.Join(certDir, "broker_ca.crt")

	cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("spy: no certificates parsed from %s", caPath)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("spy: peer sent no certificates")
			}
			parsed := make([]*x509.Certificate, 0, len(rawCerts))
			for _, raw := range rawCerts {
				p, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parse peer cert: %w", err)
				}
				parsed = append(parsed, p)
			}
			intermediates := x509.NewCertPool()
			for _, c := range parsed[1:] {
				intermediates.AddCert(c)
			}
			_, err := parsed[0].Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intermediates,
			})
			return err
		},
		MinVersion: tls.VersionTLS12,
	}, nil
}

func newMessageHandler(out *log.Logger) paho.MessageHandler {
	return func(_ paho.Client, msg paho.Message) {
		payload := msg.Payload()
		var b strings.Builder
		fmt.Fprintf(&b, "TOPIC=%s LEN=%d\n", msg.Topic(), len(payload))
		formatHexASCII(&b, payload)
		if len(payload) > 0 && (payload[0] == 0x0a || payload[0] == 0x12) {
			if req, err := proto.DecodeRPCRequest(payload); err == nil && req != nil {
				fmt.Fprintf(&b, "   RPC path=%q request_id=%q\n", req.Path, req.RequestID)
			}
		}
		out.Print(b.String())
	}
}

// formatHexASCII writes a multi-line hex + ASCII dump of data
// with 32 bytes per line and an offset prefix.
func formatHexASCII(b *strings.Builder, data []byte) {
	for i := 0; i < len(data); i += hexBytesPerLine {
		end := i + hexBytesPerLine
		if end > len(data) {
			end = len(data)
		}
		chunk := data[i:end]
		fmt.Fprintf(b, "   HEX  %04x: % x\n", i, chunk)
		fmt.Fprintf(b, "   ASCII %04x: %s\n", i, asciiPreview(chunk))
	}
}

func asciiPreview(data []byte) string {
	out := make([]byte, len(data))
	for i, c := range data {
		if c >= 0x20 && c < 0x7f {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}
