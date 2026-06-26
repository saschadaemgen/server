package mqttstore

import "strings"

// ValidTopicFilter reports whether f is a well-formed MQTT topic
// filter (the form stored in ACL rules; '+' and '#' wildcards
// allowed). Rules: '#' may appear only as the final whole level;
// '+' must occupy a whole level; no empty filter.
func ValidTopicFilter(f string) bool {
	if f == "" || len(f) > 65535 || strings.ContainsRune(f, 0) {
		return false
	}
	levels := strings.Split(f, "/")
	for i, l := range levels {
		if strings.Contains(l, "#") {
			if l != "#" || i != len(levels)-1 {
				return false
			}
		}
		if strings.Contains(l, "+") && l != "+" {
			return false
		}
	}
	return true
}

// MatchTopicFilter reports whether a concrete topic name matches an
// MQTT topic filter. Used on the PUBLISH path, where topic has no
// wildcards. Per spec, a leading '+' or '#' wildcard does not match
// topics whose first level begins with '$' (e.g. $SYS), so a device
// granted "#" still cannot reach broker internals by accident.
func MatchTopicFilter(filter, topic string) bool {
	fs := strings.Split(filter, "/")
	ts := strings.Split(topic, "/")

	if len(ts) > 0 && strings.HasPrefix(ts[0], "$") {
		if fs[0] == "#" || fs[0] == "+" {
			return false
		}
	}

	i := 0
	for ; i < len(fs); i++ {
		switch fs[i] {
		case "#":
			// Matches the remaining levels, including zero (so
			// "a/#" matches the parent topic "a").
			return true
		case "+":
			if i >= len(ts) {
				return false
			}
		default:
			if i >= len(ts) || fs[i] != ts[i] {
				return false
			}
		}
	}
	return i == len(ts)
}

// FilterCovers reports whether ACL filter a authorizes a requested
// subscription filter b: every concrete topic that b could match is
// also matched by a. Used on the SUBSCRIBE path, where the requested
// topic is itself a filter and may contain wildcards.
func FilterCovers(a, b string) bool {
	as := strings.Split(a, "/")
	bs := strings.Split(b, "/")
	i := 0
	for ; i < len(as); i++ {
		if as[i] == "#" {
			return true
		}
		if i >= len(bs) {
			return false
		}
		if as[i] == "+" {
			// A single-level wildcard cannot cover a multi-level one.
			if bs[i] == "#" {
				return false
			}
			continue
		}
		if as[i] != bs[i] {
			return false
		}
	}
	return i == len(bs)
}

// DefaultSubtree is the implicit per-device topic subtree a device
// may use without any explicit ACL rule: carvilon/<username>/#.
func DefaultSubtree(username string) string {
	return "carvilon/" + username + "/#"
}
