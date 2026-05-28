package streampublish

import (
	"io"
	"log/slog"
	"testing"
)

func TestNoop_SatisfiesInterfaceAndDoesNotPanic(t *testing.T) {
	// Compile-time: Noop satisfies StreamPublisher.
	var p StreamPublisher = NewNoop(slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.StartPublish("0c:ea:14:00:00:01", "tok-abc", "https://vps.example/whip")
	p.StopPublish("0c:ea:14:00:00:01")
}

func TestNewNoop_NilLoggerFallsBack(t *testing.T) {
	if NewNoop(nil) == nil {
		t.Fatal("NewNoop(nil) returned nil")
	}
}
