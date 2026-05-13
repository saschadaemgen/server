package eventbus

import (
	"testing"
	"time"
)

func TestBus_PublishDeliversToSubscriber(t *testing.T) {
	b := New()
	ch := b.Subscribe("0c:ea:14:42:42:42")
	defer b.Unsubscribe("0c:ea:14:42:42:42", ch)

	if n := b.Publish("0c:ea:14:42:42:42", Event{Type: "x", JSON: "{}"}); n != 1 {
		t.Errorf("Publish returned %d, want 1", n)
	}
	select {
	case ev := <-ch:
		if ev.Type != "x" {
			t.Errorf("ev.Type = %q, want x", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestBus_PublishToOtherSubjectIgnored(t *testing.T) {
	b := New()
	ch := b.Subscribe("a")
	defer b.Unsubscribe("a", ch)
	if n := b.Publish("b", Event{Type: "x"}); n != 0 {
		t.Errorf("Publish to wrong subject returned %d, want 0", n)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBus_MultipleSubscribers(t *testing.T) {
	b := New()
	ch1 := b.Subscribe("mac")
	ch2 := b.Subscribe("mac")
	defer b.Unsubscribe("mac", ch1)
	defer b.Unsubscribe("mac", ch2)
	if got := b.SubscriberCount("mac"); got != 2 {
		t.Fatalf("SubscriberCount = %d, want 2", got)
	}
	if n := b.Publish("mac", Event{Type: "y"}); n != 2 {
		t.Errorf("Publish returned %d, want 2", n)
	}
	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("subscriber missed event")
		}
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	b := New()
	ch := b.Subscribe("z")
	b.Unsubscribe("z", ch)
	if got := b.SubscriberCount("z"); got != 0 {
		t.Errorf("SubscriberCount after unsubscribe = %d, want 0", got)
	}
	// Channel is closed.
	if _, ok := <-ch; ok {
		t.Error("channel not closed after Unsubscribe")
	}
}

func TestBus_DropsOnFullBuffer(t *testing.T) {
	b := New()
	ch := b.Subscribe("slow")
	defer b.Unsubscribe("slow", ch)
	// Fill the buffer (16 events) without draining.
	for i := 0; i < SubscriberBuffer; i++ {
		b.Publish("slow", Event{Type: "x"})
	}
	// One more should be dropped.
	b.Publish("slow", Event{Type: "x"})
	if got := b.DroppedCount(); got != 1 {
		t.Errorf("DroppedCount = %d, want 1", got)
	}
}

func TestBus_PublishAll(t *testing.T) {
	b := New()
	chA := b.Subscribe("a")
	chB := b.Subscribe("b")
	defer b.Unsubscribe("a", chA)
	defer b.Unsubscribe("b", chB)
	if n := b.PublishAll([]string{"a", "b", "c"}, Event{Type: "broad"}); n != 2 {
		t.Errorf("PublishAll returned %d, want 2 (c has no subs)", n)
	}
	for _, ch := range []<-chan Event{chA, chB} {
		select {
		case ev := <-ch:
			if ev.Type != "broad" {
				t.Errorf("ev.Type = %q", ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("missed event")
		}
	}
}
