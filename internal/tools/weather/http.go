package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	DefaultUserAgent      = "AILuo/3 weather (https://github.com/projectluojia/AI-Luo-Man-ga)"
	DefaultTimeout        = 8 * time.Second
	DefaultMaxRetries     = 2
	DefaultRetryBase      = 250 * time.Millisecond
	DefaultRetryMax       = 2 * time.Second
	DefaultRequestsPerMin = 30
	maxResponseBytes      = 256 << 10
	maxRedirects          = 3
)

// HTTPDoer 是可注入的 HTTP 执行器，测试用 httptest 客户端替换。
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type ClientConfig struct {
	HTTP              HTTPDoer
	Cache             Cache
	Now               func() time.Time
	Timeout           time.Duration
	MaxRetries        int
	RetryBase         time.Duration
	RetryMax          time.Duration
	RequestsPerMinute int
	UserAgent         string
	AllowHTTP         bool
	OpenMeteoBase     string
	OpenMeteoAirBase  string
	OpenMeteoGeoBase  string
	XiaomiBase        string
	XiaomiAppKey      string
	XiaomiSign        string
	AccuWeatherBase   string
	AccuWeatherAPIKey string
}

type Client struct {
	http              HTTPDoer
	cache             Cache
	now               func() time.Time
	timeout           time.Duration
	maxRetries        int
	retryBase         time.Duration
	retryMax          time.Duration
	userAgent         string
	allowHTTP         bool
	openMeteoBase     string
	openMeteoAirBase  string
	openMeteoGeoBase  string
	xiaomiBase        string
	xiaomiAppKey      string
	xiaomiSign        string
	accuWeatherBase   string
	accuWeatherAPIKey string
	limiters          map[string]*tokenBucket
	limitersMu        sync.Mutex
	requestsPerMin    int
}

func NewClient(config ClientConfig) *Client {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	httpClient := config.HTTP
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return errors.New("too many redirects")
				}
				if req.URL.Host != via[0].URL.Host {
					return errors.New("redirect host mismatch")
				}
				return nil
			},
		}
	}
	retries := config.MaxRetries
	if retries < 0 {
		retries = 0
	}
	if retries > 5 {
		retries = 5
	}
	retryBase := config.RetryBase
	if retryBase <= 0 {
		retryBase = DefaultRetryBase
	}
	retryMax := config.RetryMax
	if retryMax <= 0 {
		retryMax = DefaultRetryMax
	}
	rpm := config.RequestsPerMinute
	if rpm <= 0 {
		rpm = DefaultRequestsPerMin
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	userAgent := strings.TrimSpace(config.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	cache := config.Cache
	if cache == nil {
		cache = NewMemoryCache()
	}
	return &Client{
		http:              httpClient,
		cache:             cache,
		now:               now,
		timeout:           timeout,
		maxRetries:        retries,
		retryBase:         retryBase,
		retryMax:          retryMax,
		userAgent:         userAgent,
		allowHTTP:         config.AllowHTTP,
		openMeteoBase:     firstNonEmpty(config.OpenMeteoBase, "https://api.open-meteo.com"),
		openMeteoAirBase:  firstNonEmpty(config.OpenMeteoAirBase, "https://air-quality-api.open-meteo.com"),
		openMeteoGeoBase:  firstNonEmpty(config.OpenMeteoGeoBase, "https://geocoding-api.open-meteo.com"),
		xiaomiBase:        firstNonEmpty(config.XiaomiBase, "https://weatherapi.market.xiaomi.com"),
		xiaomiAppKey:      firstNonEmpty(config.XiaomiAppKey, "weather20151024"),
		xiaomiSign:        firstNonEmpty(config.XiaomiSign, "zUFJoAR2ZVrDy1vF3D07"),
		accuWeatherBase:   firstNonEmpty(config.AccuWeatherBase, "https://dataservice.accuweather.com"),
		accuWeatherAPIKey: strings.TrimSpace(config.AccuWeatherAPIKey),
		limiters:          make(map[string]*tokenBucket),
		requestsPerMin:    rpm,
	}
}

func (c *Client) AccuWeatherEnabled() bool {
	return c.accuWeatherAPIKey != ""
}

func (c *Client) cached(ctx context.Context, appID, cacheKey string) (json.RawMessage, CacheEntry, bool, error) {
	if c.cache == nil || appID == "" {
		return nil, CacheEntry{}, false, nil
	}
	entry, ok, err := c.cache.GetWeather(ctx, appID, cacheKey, c.now())
	if err != nil {
		return nil, CacheEntry{}, false, err
	}
	if !ok {
		return nil, CacheEntry{}, false, nil
	}
	return append(json.RawMessage(nil), entry.Payload...), entry, true, nil
}

func (c *Client) store(ctx context.Context, appID, cacheKey, provider, revision string, payload json.RawMessage, ttl time.Duration) (CacheEntry, error) {
	fetchedAt := c.now().UTC()
	entry := CacheEntry{
		Key:            cacheKey,
		Provider:       provider,
		Payload:        append(json.RawMessage(nil), payload...),
		SourceRevision: revision,
		FetchedAt:      fetchedAt,
		ValidUntil:     fetchedAt.Add(ttl),
	}
	if c.cache == nil || appID == "" {
		return entry, nil
	}
	if err := c.cache.PutWeather(ctx, appID, entry); err != nil {
		return CacheEntry{}, err
	}
	return entry, nil
}

func (c *Client) getJSON(ctx context.Context, provider, rawURL string, dest any) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: weather request url is invalid", contracts.ErrDataIncomplete)
	}
	if parsed.Scheme != "https" && !(c.allowHTTP && parsed.Scheme == "http") {
		return fmt.Errorf("%w: weather provider url must be https", contracts.ErrDataUntrusted)
	}
	if !c.limiter(provider).allow(c.now()) {
		observe.Warn(ctx, "天气数据源触发限流",
			observe.StringAttr("provider", provider),
			observe.StringAttr("host", parsed.Host),
		)
		return fmt.Errorf("%w: weather provider rate limited", contracts.ErrDataUnavailable)
	}
	var last error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			delay := c.backoff(attempt)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return fmt.Errorf("build weather request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		started := c.now()
		resp, err := c.http.Do(req)
		if err != nil {
			last = fmt.Errorf("%w: weather provider request failed", contracts.ErrDataUnavailable)
			if !retryableTransport(err) || attempt == c.maxRetries {
				observe.Warn(ctx, "天气数据源请求失败",
					observe.StringAttr("provider", provider),
					observe.StringAttr("host", parsed.Host),
					observe.IntAttr("attempt", attempt+1),
					observe.Duration(started),
				)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return last
			}
			continue
		}
		body, readErr := readLimited(resp.Body, maxResponseBytes)
		closeErr := resp.Body.Close()
		if readErr != nil {
			last = fmt.Errorf("%w: weather provider response is too large or unreadable", contracts.ErrDataIncomplete)
			if attempt == c.maxRetries {
				return last
			}
			continue
		}
		if closeErr != nil {
			last = fmt.Errorf("close weather response: %w", closeErr)
		}
		observe.Debug(ctx, "天气数据源响应已接收",
			observe.StringAttr("provider", provider),
			observe.StringAttr("host", parsed.Host),
			observe.IntAttr("status_code", resp.StatusCode),
			observe.IntAttr("response_bytes", len(body)),
			observe.IntAttr("attempt", attempt+1),
			observe.Duration(started),
		)
		if retryableStatus(resp.StatusCode) && attempt < c.maxRetries {
			last = fmt.Errorf("%w: weather provider status %d", contracts.ErrDataUnavailable, resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%w: weather provider status %d", contracts.ErrDataUnavailable, resp.StatusCode)
		}
		if err := json.Unmarshal(body, dest); err != nil {
			return fmt.Errorf("%w: weather provider json is invalid", contracts.ErrDataIncomplete)
		}
		return nil
	}
	if last == nil {
		last = fmt.Errorf("%w: weather provider request failed", contracts.ErrDataUnavailable)
	}
	return last
}

func (c *Client) backoff(attempt int) time.Duration {
	delay := c.retryBase
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= c.retryMax {
			return c.retryMax
		}
	}
	if delay > c.retryMax {
		return c.retryMax
	}
	return delay
}

func (c *Client) limiter(provider string) *tokenBucket {
	c.limitersMu.Lock()
	defer c.limitersMu.Unlock()
	if bucket, ok := c.limiters[provider]; ok {
		return bucket
	}
	bucket := newTokenBucket(float64(c.requestsPerMin)/60, float64(c.requestsPerMin))
	c.limiters[provider] = bucket
	return bucket
}

func (c *Client) join(base, path string, query url.Values) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return "", fmt.Errorf("%w: weather provider base url is invalid", contracts.ErrDataIncomplete)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	rate   float64
	burst  float64
	last   time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	if rate <= 0 {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{tokens: burst, rate: rate, burst: burst}
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last = now
		b.tokens = b.burst
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func readLimited(body io.Reader, limit int) ([]byte, error) {
	limited := io.LimitReader(body, int64(limit)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("response exceeds size limit")
	}
	return data, nil
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func retryableTransport(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func revision(provider string, fetchedAt time.Time) string {
	return provider + "/" + fetchedAt.UTC().Format(time.RFC3339Nano)
}
