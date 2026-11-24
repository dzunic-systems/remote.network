package clientproto

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// ConvParts is what identifies a conversation for sticky routing: its
// opening system (or developer) message and its first user message, as the
// client sent them. ok is false without a user message — no key then.
func ConvParts(msgs []Message) (system, user string, ok bool) {
	sawSystem := false
	for i := range msgs {
		m := &msgs[i]
		switch m.Role {
		case "system", "developer":
			if !sawSystem {
				system, sawSystem = m.Text(), true
			}
		case "user":
			if !ok {
				user, ok = m.Text(), true
			}
		}
		if sawSystem && ok {
			break
		}
	}
	return system, user, ok
}

// HashParts is the length-prefixed SHA-256 over parts, hex — the shape of
// every sticky-routing key. Length prefixes keep ("ab","c") and ("a","bc")
// apart.
func HashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ConvHash is the content half of a conversation key, computed where the
// text is: by the router for requests it relays, by the client on the
// direct path (which then sends only this hash — the router mixes in the
// account, model and pin without ever seeing the text). "" without a user
// message.
func ConvHash(msgs []Message) string {
	system, user, ok := ConvParts(msgs)
	if !ok {
		return ""
	}
	return HashParts(system, user)
}
