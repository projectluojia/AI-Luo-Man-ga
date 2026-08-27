package weather

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

// Cache 是天气规范化结果的持久缓存端口。实现必须按 app_id 隔离。
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

// MemoryCache 是测试用内存缓存，按 app_id 隔离。过期条目视为未命中。
type MemoryCache struct {
	mu    sync.Mutex
	items map[string]CacheEntry
}

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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[appID+"\x00"+entry.Key] = cloneEntry(entry)
	return nil
}

func cloneEntry(entry CacheEntry) CacheEntry {
	entry.Payload = append(json.RawMessage(nil), entry.Payload...)
	return entry
}
