package router

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// Active mode decides a request from the latest user message that has authored
// text. Inside an agentic loop most turns carry only tool output, so there is
// nothing to send; without a memory the keyword matcher would take over mid-task
// and the model could change under the user. The cache keeps Jev's decision for
// the last authored message so those turns reuse it. Nothing is sent to Jev for
// such a turn: the lookup is by a hash of text already classified.
const (
	jevCacheTTL = 30 * time.Minute
	jevCacheMax = 256
)

type jevCacheKey [sha256.Size]byte

// jevKey hashes the stripped authored text of a user message.
func jevKey(text string) jevCacheKey {
	return sha256.Sum256([]byte(strings.TrimSpace(text)))
}

type jevCacheEntry struct {
	res JevResult
	at  time.Time
}

// jevDecisionCache is a small TTL map keyed by the hash of the authored text.
// The zero value is usable; tests inject now.
type jevDecisionCache struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[jevCacheKey]jevCacheEntry
}

func (c *jevDecisionCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *jevDecisionCache) get(k jevCacheKey) (JevResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return JevResult{}, false
	}
	if c.clock().Sub(e.at) > jevCacheTTL {
		delete(c.m, k)
		return JevResult{}, false
	}
	return e.res, true
}

func (c *jevDecisionCache) put(k jevCacheKey, res JevResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[jevCacheKey]jevCacheEntry)
	}
	now := c.clock()
	if _, exists := c.m[k]; !exists && len(c.m) >= jevCacheMax {
		c.evictLocked(now)
	}
	c.m[k] = jevCacheEntry{res: res, at: now}
}

// evictLocked drops expired entries and, if the map is still full, the oldest one.
func (c *jevDecisionCache) evictLocked(now time.Time) {
	var oldestKey jevCacheKey
	var oldest time.Time
	first := true
	for k, e := range c.m {
		if now.Sub(e.at) > jevCacheTTL {
			delete(c.m, k)
			continue
		}
		if first || e.at.Before(oldest) {
			oldestKey, oldest, first = k, e.at, false
		}
	}
	if len(c.m) >= jevCacheMax && !first {
		delete(c.m, oldestKey)
	}
}

// latestAuthoredText is the stripped authored text of the most recent user
// message that has any. It is used only as a cache key, never sent.
func latestAuthoredText(messages []MessageContent) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" || len(messages[i].PromptText) > maxJevPromptBytes {
			continue
		}
		if t := strings.TrimSpace(stripHarnessBlocks(messages[i].PromptText)); t != "" {
			return t
		}
	}
	return ""
}
