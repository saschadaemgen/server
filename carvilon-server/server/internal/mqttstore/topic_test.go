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
		// $-topic guard on the subscribe path: a leading wildcard ACL
		// filter must not cover a $SYS subscription request.
		{"#", "$SYS/#", false},
		{"#", "$SYS/broker/uptime", false},
		{"+", "$SYS/x", false},
		{"$SYS/#", "$SYS/broker/uptime", true}, // explicit $SYS rule still works
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

// TestAllowedSysGuard: even a device explicitly granted "#" cannot
// reach $SYS via subscribe or publish; an explicit $SYS rule can.
func TestAllowedSysGuard(t *testing.T) {
	hashAll := &Authz{Devices: map[string]DeviceAuthz{
		"wide": {Rules: []ACLRule{{Action: "both", TopicFilter: "#", Allow: true}}},
		"sys":  {Rules: []ACLRule{{Action: "subscribe", TopicFilter: "$SYS/#", Allow: true}}},
	}}
	if hashAll.Allowed("wide", "$SYS/#", false) {
		t.Error(`"#" subscribe rule must NOT authorize $SYS subscription`)
	}
	if hashAll.Allowed("wide", "$SYS/broker/uptime", true) {
		t.Error(`"#" publish rule must NOT authorize $SYS publish`)
	}
	if !hashAll.Allowed("wide", "carvilon/anything/x", true) {
		t.Error(`"#" rule should still grant a normal (non-$) topic`)
	}
	if !hashAll.Allowed("sys", "$SYS/broker/uptime", false) {
		t.Error("explicit $SYS/# rule must authorize $SYS subscription")
	}
}
