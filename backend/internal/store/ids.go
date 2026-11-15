package store

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a 16 hex character random identifier.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// shortID returns the first 8 characters of an id (used for media path names).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
