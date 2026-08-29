package wx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const maxCachePayloadBytes = 256 << 10

// Cache 是天气规范化结果的进程内缓存端口。isolated provider 没有宿主
// ailuo.store，缓存随进程生命周期存活；按 app_id 隔离键。
type Cache interface {
	GetWeather(ctx context.Context, appID, cacheKey string, now time.Time) (CacheEntry, bool, error)
	PutWeather(ctx context.Context, appID string, entry CacheEntry) error
}

// CacheEntry 保存已规范化的天气载荷，不保存第三方原始响应。
type CacheEntry struct {
	Key            string
	Provider       string
	Payload        json.RawMessage
	SourceRevision string
	FetchedAt      time.Time
	ValidUntil     time.Time
}

func CacheKey(provider, kind string, parts ...any) string {
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%s\n%s", provider, kind)
	for _, part := range parts {
		_, _ = fmt.Fprintf(digest, "\n%v", part)
	}
	return provider + "." + kind + "." + hex.EncodeToString(digest.Sum(nil))[:24]
}

// MemoryCache 是带容量上界的进程内缓存，按 app_id 隔离。过期条目视为未命中；
// 超过容量时逐出最早的条目，保证长时间驻留的进程不无界增长。
type MemoryCache struct {
	mu    sync.Mutex
	items map[string]CacheEntry
	order []string
}

const maxMemoryCacheEntries = 512

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{items: make(map[string]CacheEntry)}
}

func (c *MemoryCache) GetWeather(_ context.Context, appID, cacheKey string, now time.Time) (CacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[appID+"\x00"+cacheKey]
	if !ok || !now.Before(entry.ValidUntil) {
		return CacheEntry{}, false, nil
	}
	return cloneEntry(entry), true, nil
}

func (c *MemoryCache) PutWeather(_ context.Context, appID string, entry CacheEntry) error {
	if appID == "" || entry.Key == "" || len(entry.Payload) == 0 || len(entry.Payload) > maxCachePayloadBytes {
		return fmt.Errorf("%w: weather cache entry is invalid", ErrInvalidRequest)
	}
	key := appID + "\x00" + entry.Key
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
		for len(c.order) > maxMemoryCacheEntries {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.items, oldest)
		}
	}
	c.items[key] = cloneEntry(entry)
	return nil
}

func cloneEntry(entry CacheEntry) CacheEntry {
	entry.Payload = append(json.RawMessage(nil), entry.Payload...)
	return entry
}
