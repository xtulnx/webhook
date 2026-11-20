package network

import (
	"crypto/hmac"
	"maps"
	"strings"
	"time"
)

const shareReconnectWindow = time.Minute
const maxShareRegistrationsPerNode = 32

type shareRegistration struct {
	control      *controlConn
	disconnected time.Time
}

// registerShareLocked reserves the public URL segments for this process.
// The existing per-process share key proves ownership on reconnect.
func (h *Hub) registerShareLocked(node string, control *controlConn, now time.Time) bool {
	slots := h.shares[node]
	active := slots[:0]
	for _, slot := range slots {
		c := slot.control
		if now.Before(c.shareExpiry) && (slot.disconnected.IsZero() || now.Before(slot.disconnected.Add(shareReconnectWindow))) {
			active = append(active, slot)
		} else if slot.disconnected.IsZero() && c.shareIDs != nil && c.conn != nil {
			_ = c.conn.Close()
		}
	}
	clear(slots[len(active):])
	h.shares[node] = active
	for _, slot := range active {
		previous := slot.control
		if hmac.Equal(control.shareKey, previous.shareKey) {
			if !maps.Equal(control.shareIDs, previous.shareIDs) || (control.shareIDs == nil) != (previous.shareIDs == nil) ||
				!maps.Equal(control.shareNames, previous.shareNames) ||
				control.shareBasic != previous.shareBasic || !control.shareExpiry.Equal(previous.shareExpiry) ||
				!sameShareID(control.shareLinkID, previous.shareLinkID) {
				return false
			}
			control.shareSlot = slot
			slot.control = control
			slot.disconnected = time.Time{}
			if previous.conn != nil {
				_ = previous.conn.Close()
			}
			return true
		}
		if shareIDsOverlap(previous.shareIDs, control.shareIDs) {
			return false
		}
	}
	if len(active) >= maxShareRegistrationsPerNode {
		return false
	}
	slot := &shareRegistration{control: control}
	control.shareSlot = slot
	h.shares[node] = append(active, slot)
	return true
}

func sameShareID(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func shareIDsOverlap(a, b map[string]bool) bool {
	// Older and mixed-service registrations have no declared IDs. An empty
	// link ID also owns the whole node's public share namespace.
	if a == nil || b == nil || a[""] || b[""] {
		return true
	}
	for id := range a {
		if b[id] {
			return true
		}
	}
	return false
}

func (h *Hub) controlActiveLocked(node string, c *controlConn) bool {
	if c == nil {
		return false
	}
	if c.shareIDs == nil {
		return h.controls[node] == c
	}
	return c.shareSlot.control == c && c.shareSlot.disconnected.IsZero() && time.Now().Before(c.shareExpiry)
}

func (h *Hub) shareRouteLocked(node, rest string) (*controlConn, string) {
	linkID, filePath, _ := strings.Cut(rest, "/")
	name, _, _ := strings.Cut(filePath, "/")
	for _, slot := range h.shares[node] {
		c := slot.control
		if !slot.disconnected.IsZero() || !time.Now().Before(c.shareExpiry) || !h.controlActiveLocked(node, c) {
			continue
		}
		if c.shareLinkID != nil && *c.shareLinkID == "" {
			name, _, _ = strings.Cut(rest, "/")
			if c.allowsShareName(name) {
				return c, rest
			}
			return nil, ""
		}
		if (c.shareIDs == nil || c.shareIDs[linkID]) && c.allowsShareName(name) &&
			hmac.Equal([]byte(linkID), []byte(shareLinkID(c.shareKey, name, c.shareBasic, c.shareLinkID))) {
			return c, filePath
		}
	}
	return nil, ""
}

func (c *controlConn) allowsShareName(name string) bool {
	if c.shareNames != nil {
		return c.shareNames[name]
	}
	return validName(name)
}

func (h *Hub) nodeControlLocked(node string) *controlConn {
	if c := h.controls[node]; c != nil {
		return c
	}
	for _, slot := range h.shares[node] {
		if h.controlActiveLocked(node, slot.control) {
			return slot.control
		}
	}
	return nil
}

func (h *Hub) serviceControlLocked(node, service string) *controlConn {
	control := h.controls[node]
	if service != "files" || (control != nil && control.services[service]) {
		return control
	}
	for _, slot := range h.shares[node] {
		c := slot.control
		if !h.controlActiveLocked(node, c) {
			continue
		}
		if control != nil && control.services[service] {
			return nil
		}
		control = c
	}
	return control
}
