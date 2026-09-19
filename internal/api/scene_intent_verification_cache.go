package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// VerifiedProgramCache is a bounded content-addressed proof cache. It never
// trusts the signed digest by itself: the raw program hash is part of the key,
// and an entry is added only after blueProgramDigest has passed.
type VerifiedProgramCache struct {
	mu      sync.Mutex
	entries map[string]struct{}
	order   []string
}

const maxVerifiedPrograms = 128

func NewVerifiedProgramCache() *VerifiedProgramCache {
	return &VerifiedProgramCache{entries: make(map[string]struct{})}
}

func (c *VerifiedProgramCache) key(expectedDigest string, program []byte) string {
	rawDigest := sha256.Sum256(program)
	return expectedDigest + ":" + hex.EncodeToString(rawDigest[:])
}

func (c *VerifiedProgramCache) contains(expectedDigest string, program []byte) bool {
	if c == nil {
		return false
	}
	key := c.key(expectedDigest, program)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	return ok
}

func (c *VerifiedProgramCache) add(expectedDigest string, program []byte) {
	if c == nil {
		return
	}
	key := c.key(expectedDigest, program)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	c.entries[key] = struct{}{}
	c.order = append(c.order, key)
	if len(c.order) > maxVerifiedPrograms {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}
