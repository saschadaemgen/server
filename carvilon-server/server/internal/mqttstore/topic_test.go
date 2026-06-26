package mqttstore

import "testing"

func TestValidTopicFilter(t *testing.T) {
	ok := []string{"a", "a/b", "a/+/c", "a/#", "#", "+", "+/+", "carvilon/eg/#", "$SYS/#"}
	for _, f := range ok {
		if !ValidTopicFilter(f) {
			t.Errorf("ValidTopicFilter(%q) = false, want true", f)
		}
	}
	bad := []string{"", "a/#/b", "a#", "a/b#", "a/+b", "a+/b", "a/b+c"}
	for _, f := range bad {
		if ValidTopicFilter(f) {
			t.Errorf("ValidTopicFilter(%q) = true, want false", f)
		}
	}
}

func TestMatchTopicFilter(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"a/+/c", "a/b/c", true},
		{"a/+/c", "a/b/d", false},
		{"a/+/c", "a/b", false},
		{"a/#", "a", true},     // # matches the parent level
		{"a/#", "a/b/c", true},
		{"#", "a/b/c", true},
		{"a/b", "a/b/c", false},
		{"a/b/c", "a/b", false},
		// $-topic guard: leading wildcard must not match $SYS.
		{"#", "$SYS/broker/uptime", false},
		{"+/broker", "$SYS/broker", false},
		{"$SYS/#", "$SYS/broker/uptime", true},
	}
	for _, c := range cases {
		if got := MatchTopicFilter(c.filter, c.topic); got != c.want {
			t.Errorf("MatchTopicFilter(%q,%q) = %v, want %v", c.filter, c.topic, got, c.want)
		}
	}
}

func TestFilterCovers(t *testing.T) {
	cases := []struct {
		acl, req string
		want     bool
	}{
		{"#", "a/b/c", true},
		{"#", "#", true},
		{"carvilon/u/#", "carvilon/u/eg/+", true},
		{"carvilon/u/#", "carvilon/#", false},
		{"carvilon/u/#", "carvilon/u", true},
		{"a/+", "a/b", true},
		{"a/+", "a/+", true},
		{"a/+", "a/#", false}, // + cannot cover #
		{"a/b", "a/+", false},
		{"a/b", "a/b/c", false},
	}
	for _, c := range cases {
		if got := FilterCovers(c.acl, c.req); got != c.want {
			t.Errorf("FilterCovers(%q,%q) = %v, want %v", c.acl, c.req, got, c.want)
		}
	}
}

func TestAllowedDefaultSubtreeAndDeny(t *testing.T) {
	az := &Authz{Pepper: "", Devices: map[string]DeviceAuthz{
		"flur": {Rules: []ACLRule{
			{Action: "publish", TopicFilter: "shared/cmd", Allow: true},
			{Action: "both", TopicFilter: "carvilon/flur/secret", Allow: false},
		}},
	}}

	// Implicit own subtree, publish + subscribe.
	if !az.Allowed("flur", "carvilon/flur/button", true) {
		t.Error("publish to own subtree should be allowed")
	}
	if !az.Allowed("flur", "carvilon/flur/#", false) {
		t.Error("subscribe to own subtree should be allowed")
	}
	// Explicit allow outside the subtree.
	if !az.Allowed("flur", "shared/cmd", true) {
		t.Error("explicit allow rule should permit publish")
	}
	// Explicit deny wins even though it is inside the own subtree.
	if az.Allowed("flur", "carvilon/flur/secret", true) {
		t.Error("explicit deny must override the default-subtree allow")
	}
	// Default-deny for anything else.
	if az.Allowed("flur", "other/topic", true) {
		t.Error("unlisted topic must be denied")
	}
	// Unknown device denied.
	if az.Allowed("ghost", "carvilon/ghost/x", true) {
		t.Error("unknown device must be denied")
	}
}
