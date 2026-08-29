package weather

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeErrorBody struct {
	io.Reader
	err error
}

func (b closeErrorBody) Close() error { return b.err }

func TestHTTPRejectsOriginChangingRedirect(t *testing.T) {
	client := NewClient(ClientConfig{})
	httpClient, ok := client.http.(*http.Client)
	if !ok || httpClient.CheckRedirect == nil {
		t.Fatal("default HTTP client did not configure redirect policy")
	}
	from := &http.Request{URL: &url.URL{Scheme: "https", Host: "weather.example"}}
	to := &http.Request{URL: &url.URL{Scheme: "http", Host: "weather.example"}}
	if err := httpClient.CheckRedirect(to, []*http.Request{from}); err == nil {
		t.Fatal("HTTPS to HTTP redirect was accepted")
	}
}

func TestHTTPClassifiesResponseCloseErrorsAndRetries(t *testing.T) {
	closeErr := errors.New("close failed")
	var calls atomic.Int32
	client := NewClient(ClientConfig{
		HTTP: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return &http.Response{StatusCode: http.StatusOK, Body: closeErrorBody{Reader: strings.NewReader(`{"ok":true}`), err: closeErr}, Request: request}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request}, nil
		}),
		AllowHTTP: true, MaxRetries: 1, RetryBase: time.Millisecond, RetryMax: time.Millisecond,
	})
	var dest map[string]any
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, "http://weather.example", &dest); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || dest["ok"] != true {
		t.Fatalf("calls=%d dest=%v", calls.Load(), dest)
	}

	client = NewClient(ClientConfig{
		HTTP: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: closeErrorBody{Reader: strings.NewReader(`{}`), err: closeErr}, Request: request}, nil
		}),
		AllowHTTP: true,
	})
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, "http://weather.example", &dest); !errors.Is(err, contracts.ErrDataIncomplete) {
		t.Fatalf("got %v, want incomplete close error", err)
	}
}

func TestHTTPRetriesRetryableStatusThenSucceeds(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") == "" {
			t.Errorf("missing User-Agent")
		}
		n := calls.Add(1)
		if n < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{HTTP: server.Client(), AllowHTTP: true, MaxRetries: 2, RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond})
	var dest map[string]any
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || dest["ok"] != true {
		t.Fatalf("calls=%d dest=%v", calls.Load(), dest)
	}
}

func TestHTTPDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{HTTP: server.Client(), AllowHTTP: true, MaxRetries: 2, RetryBase: time.Millisecond})
	var dest map[string]any
	err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest)
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("got %v, want data unavailable", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
}

func TestHTTPHonorsCancel(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{HTTP: server.Client(), AllowHTTP: true, MaxRetries: 0})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var dest map[string]any
	if err := client.getJSON(ctx, ProviderOpenMeteo, server.URL, &dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled", err)
	}
}

func TestHTTPRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(bytes.Repeat([]byte("a"), maxResponseBytes+8))
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{HTTP: server.Client(), AllowHTTP: true, MaxRetries: 0})
	var dest map[string]any
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest); !errors.Is(err, contracts.ErrDataIncomplete) {
		t.Fatalf("got %v, want incomplete", err)
	}
}

func TestHTTPRateLimitReturnsUnavailable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	client := NewClient(ClientConfig{
		HTTP: server.Client(), AllowHTTP: true, RequestsPerMinute: 1,
		Now: func() time.Time { return now },
	})
	var dest map[string]any
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest); err != nil {
		t.Fatal(err)
	}
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest); !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("got %v, want rate limited unavailable", err)
	}
}

func TestHTTPLogsDoNotContainAPIKey(t *testing.T) {
	buffer := &bytes.Buffer{}
	if _, err := observe.Configure(observe.Config{Service: "test", Environment: "test", Format: "console", Writer: buffer}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("apikey") != "super-secret-key" {
			t.Errorf("provider did not receive apikey")
		}
		_, _ = io.WriteString(writer, `{"Key":"loc","LocalizedName":"珞珈山","GeoPosition":{"Latitude":30.538,"Longitude":114.362}}`)
	}))
	t.Cleanup(server.Close)
	client := NewClient(ClientConfig{
		HTTP: server.Client(), AllowHTTP: true, AccuWeatherAPIKey: "super-secret-key", AccuWeatherBase: server.URL,
	})
	ctx := observe.With(t.Context(), slog.String("apikey", "super-secret-key"), slog.String("sign", "xiaomi-sign"))
	if _, err := client.accuLocation(ctx, "campus-services", LocationQuery{Latitude: 30.538, Longitude: 114.362, Hours: 1}); err != nil {
		t.Fatal(err)
	}
	output := buffer.String()
	if strings.Contains(output, "super-secret-key") || strings.Contains(output, "xiaomi-sign") {
		t.Fatalf("secret leaked into logs: %s", output)
	}
}

func TestCacheHitSkipsHTTP(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = writer.Write([]byte(openMeteoForecastJSON))
	}))
	t.Cleanup(server.Close)
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	client := testClient(server, now)
	query := LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude, Hours: 2}
	first, err := client.OpenMeteoForecast(t.Context(), "campus-services", query)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.OpenMeteoForecast(t.Context(), "campus-services", query)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || first.DataStatus.CacheHit || !second.DataStatus.CacheHit {
		t.Fatalf("calls=%d firstHit=%v secondHit=%v", calls.Load(), first.DataStatus.CacheHit, second.DataStatus.CacheHit)
	}
	other, err := client.OpenMeteoForecast(t.Context(), "other-app", query)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || other.DataStatus.CacheHit {
		t.Fatalf("app isolation failed calls=%d hit=%v", calls.Load(), other.DataStatus.CacheHit)
	}
}

func TestExpiredCacheIsNotReturnedAsFact(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	clock := now
	cache := NewMemoryCache()
	payload, _ := json.Marshal(ForecastResult{
		DataStatus: NewDataStatus(ProviderOpenMeteo, "open-meteo-forecast", "rev", now, now.Add(time.Minute), false),
		Location:   defaultLocation(),
		Current:    Current{ObservedAt: now, TemperatureC: 1, WeatherText: "晴"},
		Hourly:     []HourlyPoint{{Time: now, TemperatureC: 1, WeatherText: "晴"}},
	})
	query := LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude, Hours: DefaultHours}
	_ = query.NormalizeAndValidate()
	if err := cache.PutWeather(t.Context(), "campus-services", CacheEntry{
		Key:      CacheKey(ProviderOpenMeteo, "forecast", query.Latitude, query.Longitude, query.Hours),
		Provider: ProviderOpenMeteo, Payload: payload, SourceRevision: "rev",
		FetchedAt: now, ValidUntil: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Minute)
	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(failing.Close)
	client := NewClient(ClientConfig{
		HTTP: failing.Client(), Cache: cache, AllowHTTP: true, OpenMeteoBase: failing.URL,
		Now: func() time.Time { return clock }, MaxRetries: 0,
	})
	_, err := client.OpenMeteoForecast(t.Context(), "campus-services", query)
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("got %v, want unavailable rather than stale cache", err)
	}
}
