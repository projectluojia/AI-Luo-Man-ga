package wx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const openMeteoForecastJSON = `{
  "latitude":30.538,"longitude":114.362,"timezone":"Asia/Shanghai",
  "current":{"time":"2026-08-27T08:00","temperature_2m":31.2,"apparent_temperature":34.1,"relative_humidity_2m":62,"precipitation":0,"weather_code":1,"wind_speed_10m":3.4,"wind_direction_10m":140,"visibility":10000},
  "hourly":{"time":["2026-08-27T08:00","2026-08-27T09:00"],"temperature_2m":[31.2,32.0],"relative_humidity_2m":[62,58],"precipitation":[0,0],"precipitation_probability":[0,10],"weather_code":[1,2],"wind_speed_10m":[3.4,3.8]}
}`

const openMeteoAirJSON = `{
  "latitude":30.538,"longitude":114.362,"timezone":"Asia/Shanghai",
  "current":{"time":"2026-08-27T08:00","us_aqi":42,"european_aqi":30,"pm2_5":12.5,"pm10":20.1}
}`

const openMeteoGeoJSON = `{"results":[{"name":"武汉大学","latitude":30.5383,"longitude":114.3617,"timezone":"Asia/Shanghai","country":"中国","admin1":"湖北省"}]}`

const xiaomiJSON = `{
  "current":{"temperature":{"value":30},"humidity":{"value":70},"weather":"多云","pubTime":"2026-08-27T08:00:00Z"},
  "forecastHourly":{"pubTime":"2026-08-27T08:00:00Z","temperature":{"value":[30,31]},"weather":{"value":["多云","晴"]}},
  "aqi":{"aqi":"85","primary":"PM2.5","pubTime":"2026-08-27T08:00:00Z"},
  "alerts":[{"alertId":"wh-1","title":"武汉市气象台发布高温黄色预警","level":"黄色","type":"高温","detail":"午后高温","pubTime":"2026-08-27T06:00:00Z"}]
}`

func testClient(server *httptest.Server, now time.Time) *Client {
	return NewClient(ClientConfig{
		HTTP: server.Client(), Cache: NewMemoryCache(), AllowHTTP: true,
		OpenMeteoBase: server.URL, OpenMeteoAirBase: server.URL, OpenMeteoGeoBase: server.URL,
		XiaomiBase: server.URL, AccuWeatherBase: server.URL, AccuWeatherAPIKey: "test-key",
		Now: func() time.Time { return now }, MaxRetries: 0, RequestsPerMinute: 600,
	})
}

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

func TestHTTPRejectsNonHTTPS(t *testing.T) {
	client := NewClient(ClientConfig{AllowHTTP: false})
	var dest map[string]any
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, "http://weather.example", &dest); !errors.Is(err, ErrDataUntrusted) {
		t.Fatalf("got %v, want untrusted", err)
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
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, "http://weather.example", &dest); !errors.Is(err, ErrDataIncomplete) {
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
		if calls.Add(1) < 3 {
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
	if !errors.Is(err, ErrDataUnavailable) {
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
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest); !errors.Is(err, ErrDataIncomplete) {
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
	if err := client.getJSON(t.Context(), ProviderOpenMeteo, server.URL, &dest); !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("got %v, want rate limited unavailable", err)
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
	clock := now.Add(2 * time.Minute)
	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(failing.Close)
	client := NewClient(ClientConfig{
		HTTP: failing.Client(), Cache: cache, AllowHTTP: true, OpenMeteoBase: failing.URL,
		Now: func() time.Time { return clock }, MaxRetries: 0,
	})
	_, err := client.OpenMeteoForecast(t.Context(), "campus-services", query)
	if !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("got %v, want unavailable rather than stale cache", err)
	}
}

func TestOpenMeteoNormalizesForecastAndAQI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "air-quality") {
			_, _ = io.WriteString(writer, openMeteoAirJSON)
			return
		}
		_, _ = io.WriteString(writer, openMeteoForecastJSON)
	}))
	t.Cleanup(server.Close)
	client := testClient(server, now)
	forecast, err := client.OpenMeteoForecast(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude, Hours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if forecast.Current.TemperatureC != 31.2 || forecast.Current.WeatherText != "大部晴朗" || len(forecast.Hourly) != 2 {
		t.Fatalf("forecast=%#v", forecast)
	}
	if _, err := forecast.DataStatus.Govern(now); err != nil {
		t.Fatal(err)
	}
	aqi, err := client.OpenMeteoAirQuality(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude})
	if err != nil || aqi.AQI.Index != 42 || aqi.AQI.Category != "优" {
		t.Fatalf("aqi=%#v err=%v", aqi, err)
	}
}

func TestOpenMeteoUsesEuropeanAQICategory(t *testing.T) {
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	query := LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude}
	european := 30
	result, err := normalizeOpenMeteoAir(query, openMeteoAirResponse{
		Current: struct {
			Time        string   `json:"time"`
			USAQI       *int     `json:"us_aqi"`
			EuropeanAQI *int     `json:"european_aqi"`
			PM25        *float64 `json:"pm2_5"`
			PM10        *float64 `json:"pm10"`
		}{EuropeanAQI: &european},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.AQI.Scale != "european" || result.AQI.Category != "良" {
		t.Fatalf("aqi=%+v", result.AQI)
	}
}

func TestOpenMeteoRejectsIncompletePayload(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"latitude":1,"longitude":2,"current":{}}`)
	}))
	t.Cleanup(server.Close)
	client := testClient(server, time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC))
	if _, err := client.OpenMeteoForecast(t.Context(), "campus-services", LocationQuery{Latitude: 1, Longitude: 2, Hours: 1}); !errors.Is(err, ErrDataIncomplete) {
		t.Fatalf("got %v, want incomplete", err)
	}
}

func TestOpenMeteoGeocodeAndUnknownPlace(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("name") == "nowhere-place" {
			_, _ = io.WriteString(writer, `{"results":[]}`)
			return
		}
		_, _ = io.WriteString(writer, openMeteoGeoJSON)
	}))
	t.Cleanup(server.Close)
	client := testClient(server, time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC))
	found, err := client.OpenMeteoGeocode(t.Context(), "campus-services", GeocodeQuery{Place: "武汉大学"})
	if err != nil || found.Location.Place != "武汉大学" {
		t.Fatalf("found=%#v err=%v", found, err)
	}
	if _, err := client.OpenMeteoGeocode(t.Context(), "campus-services", GeocodeQuery{Place: "nowhere-place"}); !errors.Is(err, ErrPlaceNotFound) {
		t.Fatalf("got %v, want place not found", err)
	}
}

func TestXiaomiNormalizesHourlyAQIAndAlerts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, xiaomiJSON)
	}))
	t.Cleanup(server.Close)
	client := testClient(server, now)
	result, err := client.XiaomiFetch(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude, Hours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Current.TemperatureC != 30 || result.AQI == nil || result.AQI.Index != 85 || len(result.Alerts) != 1 {
		t.Fatalf("xiaomi=%#v", result)
	}
	if result.Alerts[0].Severity != "watch" || result.Hourly[1].WeatherText != "晴" {
		t.Fatalf("alerts/hourly=%#v", result)
	}
}

func TestAccuWeatherRequiresKeyAndNormalizes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("apikey") == "" {
			t.Errorf("missing apikey")
		}
		switch {
		case strings.Contains(request.URL.Path, "geoposition"):
			_, _ = io.WriteString(writer, `{"Key":"123","LocalizedName":"武昌","TimeZone":{"Name":"Asia/Shanghai"},"Country":{"LocalizedName":"中国"},"AdministrativeArea":{"LocalizedName":"湖北"},"GeoPosition":{"Latitude":30.538,"Longitude":114.362}}`)
		case strings.Contains(request.URL.Path, "currentconditions"):
			_, _ = io.WriteString(writer, `[{"LocalObservationDateTime":"2026-08-27T08:00:00+08:00","WeatherText":"晴","WeatherIcon":1,"RelativeHumidity":55,"Temperature":{"Metric":{"Value":30.5}},"RealFeelTemperature":{"Metric":{"Value":33}},"Wind":{"Direction":{"Degrees":90},"Speed":{"Metric":{"Value":10.8,"Unit":"km/h"}}},"PrecipitationSummary":{"Precipitation":{"Metric":{"Value":0}}},"Visibility":{"Metric":{"Value":10,"Unit":"km"}}}]`)
		case strings.Contains(request.URL.Path, "hourly"):
			_, _ = io.WriteString(writer, `[{"DateTime":"2026-08-27T08:00:00+08:00","WeatherIcon":1,"IconPhrase":"晴","Temperature":{"Value":30.5},"RelativeHumidity":55,"PrecipitationProbability":0,"Rain":{"Value":0},"Wind":{"Speed":{"Value":10.8,"Unit":"km/h"}}}]`)
		case strings.Contains(request.URL.Path, "alerts"):
			_, _ = io.WriteString(writer, `[{"AlertID":9,"Description":{"Localized":"高温预警"},"Category":"Heat","Type":"Warning","Source":"CMA","Area":[{"StartTime":"2026-08-27T06:00:00+08:00","EndTime":"2026-08-27T20:00:00+08:00"}]}]`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	disabled := NewClient(ClientConfig{AllowHTTP: true, AccuWeatherBase: server.URL})
	if _, err := disabled.AccuWeatherFetch(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude}); !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("got %v, want disabled", err)
	}
	client := testClient(server, now)
	forecast, err := client.AccuWeatherFetch(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude, Hours: 1})
	if err != nil || forecast.Current.WeatherText != "晴" || forecast.Location.Place != "武昌" {
		t.Fatalf("forecast=%#v err=%v", forecast, err)
	}
	alerts, err := client.AccuWeatherAlerts(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude})
	if err != nil || len(alerts.Alerts) != 1 || alerts.Alerts[0].Severity != "warning" {
		t.Fatalf("alerts=%#v err=%v", alerts, err)
	}
}

func TestAccuWeatherHourlyStrictCount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "geoposition") {
			_, _ = io.WriteString(writer, `{"Key":"123","LocalizedName":"武昌","GeoPosition":{"Latitude":30.538,"Longitude":114.362}}`)
			return
		}
		// 只返回 1 个小时点，但请求 2 小时：静默截断被禁止。
		_, _ = io.WriteString(writer, `[{"DateTime":"2026-08-27T08:00:00+08:00","WeatherIcon":1,"IconPhrase":"晴","Temperature":{"Value":30.5}}]`)
	}))
	t.Cleanup(server.Close)
	client := testClient(server, now)
	if _, err := client.AccuWeatherFetch(t.Context(), "campus-services", LocationQuery{Latitude: DefaultLatitude, Longitude: DefaultLongitude, Hours: 2}); !errors.Is(err, ErrDataIncomplete) {
		t.Fatalf("got %v, want incomplete for truncated hourly", err)
	}
}

func TestLookupRequestDefaultsToCampus(t *testing.T) {
	t.Parallel()
	var request LookupRequest
	if err := request.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if request.Place != DefaultPlace || *request.Latitude != DefaultLatitude || request.Source != ProviderAuto {
		t.Fatalf("%#v", request)
	}
}

func TestLookupRequestRejectsUnknownFieldsViaDecoder(t *testing.T) {
	t.Parallel()
	_, err := decodeLookup(json.RawMessage(`{"place":"武汉","unknown":true}`))
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != CodeInvalidArguments {
		t.Fatalf("got %v, want invalid_arguments", err)
	}
}

func TestDecodeLookupRejectsTrailingData(t *testing.T) {
	t.Parallel()
	if _, err := decodeLookup(json.RawMessage(`{"hours":2}{"hours":3}`)); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestMemoryCacheEvictsOldest(t *testing.T) {
	t.Parallel()
	cache := NewMemoryCache()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	for i := 0; i < maxMemoryCacheEntries+1; i++ {
		key := "k" + strconv.Itoa(i)
		if err := cache.PutWeather(t.Context(), "app", CacheEntry{
			Key: key, Provider: ProviderOpenMeteo, Payload: json.RawMessage(`{}`),
			SourceRevision: "rev", FetchedAt: now, ValidUntil: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := cache.GetWeather(t.Context(), "app", "k0", now); ok {
		t.Fatal("oldest entry survived eviction")
	}
	if _, ok, _ := cache.GetWeather(t.Context(), "app", "k"+strconv.Itoa(maxMemoryCacheEntries), now); !ok {
		t.Fatal("newest entry was evicted")
	}
}

func TestDataStatusGovernRejectsIncompleteUntrustedAndExpired(t *testing.T) {
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	if _, err := (DataStatus{Source: "s"}).Govern(now); !errors.Is(err, ErrDataIncomplete) {
		t.Fatalf("incomplete: got %v", err)
	}
	untrusted := NewDataStatus(ProviderOpenMeteo, "src", "rev", now, now.Add(time.Minute), false)
	untrusted.Authoritative = false
	if _, err := untrusted.Govern(now); !errors.Is(err, ErrDataUntrusted) {
		t.Fatalf("untrusted: got %v", err)
	}
	expired := NewDataStatus(ProviderOpenMeteo, "src", "rev", now.Add(-2*time.Minute), now.Add(-time.Minute), false)
	if _, err := expired.Govern(now); !errors.Is(err, ErrDataExpired) {
		t.Fatalf("expired: got %v", err)
	}
	governed, err := NewDataStatus(ProviderOpenMeteo, "src", "rev", now, now.Add(time.Minute), false).Govern(now)
	if err != nil || governed.State != DataStateAuthoritativeFresh {
		t.Fatalf("fresh: got %v err=%v", governed, err)
	}
}
