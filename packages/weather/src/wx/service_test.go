package wx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serviceTestClient(server *httptest.Server, now time.Time, accuKey string) *Client {
	return NewClient(ClientConfig{
		HTTP: server.Client(), Cache: NewMemoryCache(), AllowHTTP: true,
		OpenMeteoBase: server.URL, OpenMeteoAirBase: server.URL, OpenMeteoGeoBase: server.URL,
		XiaomiBase: server.URL, AccuWeatherBase: server.URL, AccuWeatherAPIKey: accuKey,
		Now: func() time.Time { return now }, MaxRetries: 0, RequestsPerMinute: 600,
	})
}

func decodeView(t *testing.T, raw json.RawMessage, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestServiceDefaultsGeocodeAndFallback(t *testing.T) {
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	var openMeteoDown bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/v1/forecast"):
			if openMeteoDown {
				writer.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = io.WriteString(writer, `{
			  "latitude":30.538,"longitude":114.362,"timezone":"Asia/Shanghai",
			  "current":{"time":"2026-08-27T08:00","temperature_2m":31.2,"weather_code":1,"relative_humidity_2m":60},
			  "hourly":{"time":["2026-08-27T08:00"],"temperature_2m":[31.2],"weather_code":[1]}
			}`)
		case strings.Contains(request.URL.Path, "air-quality"):
			_, _ = io.WriteString(writer, `{"latitude":30.538,"longitude":114.362,"timezone":"Asia/Shanghai","current":{"time":"2026-08-27T08:00","us_aqi":40,"pm2_5":11}}`)
		case strings.Contains(request.URL.Path, "/v1/search"):
			_, _ = io.WriteString(writer, `{"results":[{"name":"信息学部","latitude":30.53,"longitude":114.357,"timezone":"Asia/Shanghai","country":"中国","admin1":"湖北"}]}`)
		case strings.Contains(request.URL.Path, "/wtr-v3/weather/all"):
			_, _ = io.WriteString(writer, `{
			  "current":{"temperature":{"value":29},"weather":"晴","pubTime":"2026-08-27T08:00:00Z"},
			  "forecastHourly":{"pubTime":"2026-08-27T08:00:00Z","temperature":{"value":[29]},"weather":{"value":["晴"]}},
			  "aqi":{"aqi":"70"},
			  "alerts":[{"title":"雷电黄色预警","level":"黄色"}]
			}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	service := NewServiceWithClock(serviceTestClient(server, now, ""), func() time.Time { return now })

	currentRaw, err := service.Current(t.Context(), "campus-services", []byte(`{}`))
	current := decodeView(t, currentRaw, err)
	if current["current"].(map[string]any)["temperature_c"].(float64) != 31.2 {
		t.Fatalf("current=%v", current)
	}
	if current["location"].(map[string]any)["place"] != DefaultPlace {
		t.Fatalf("default place=%v", current["location"])
	}

	namedRaw, err := service.Hourly(t.Context(), "campus-services", []byte(`{"place":"信息学部","hours":1}`))
	named := decodeView(t, namedRaw, err)
	if named["location"].(map[string]any)["place"] != "信息学部" {
		t.Fatalf("named location=%v", named["location"])
	}

	aqiRaw, err := service.AQI(t.Context(), "campus-services", []byte(`{}`))
	aqi := decodeView(t, aqiRaw, err)
	if aqi["aqi"].(map[string]any)["index"].(float64) != 40 {
		t.Fatalf("aqi=%v", aqi)
	}

	alertsRaw, err := service.Alerts(t.Context(), "campus-services", []byte(`{}`))
	alerts := decodeView(t, alertsRaw, err)
	if list, ok := alerts["alerts"].([]any); !ok || len(list) != 1 {
		t.Fatalf("alerts=%v", alerts)
	}

	openMeteoDown = true
	// 新 app 绕开缓存：fallback 命中小米。
	fallbackRaw, err := service.Current(t.Context(), "other-app", []byte(`{"latitude":30.4,"longitude":114.3,"source":"auto"}`))
	fallback := decodeView(t, fallbackRaw, err)
	if fallback["data_status"].(map[string]any)["provider"] != ProviderXiaomi {
		t.Fatalf("fallback provider=%v", fallback["data_status"])
	}

	_, err = service.Current(t.Context(), "campus-services", []byte(`{"unknown":1}`))
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != CodeInvalidArguments {
		t.Fatalf("got %v, want invalid_arguments", err)
	}
	_, err = service.Alerts(t.Context(), "campus-app-2", []byte(`{"source":"openmeteo","latitude":30.4,"longitude":114.3}`))
	if !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("openmeteo alerts got %v, want unavailable", err)
	}
}

func TestServiceAccuWeatherDisabledIsSkipped(t *testing.T) {
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "forecast") {
			_, _ = io.WriteString(writer, `{
			  "latitude":30.538,"longitude":114.362,"timezone":"Asia/Shanghai",
			  "current":{"time":"2026-08-27T08:00","temperature_2m":20,"weather_code":0},
			  "hourly":{"time":["2026-08-27T08:00"],"temperature_2m":[20],"weather_code":[0]}
			}`)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	service := NewServiceWithClock(serviceTestClient(server, now, ""), func() time.Time { return now })
	_, err := service.Current(t.Context(), "campus-services", []byte(`{"source":"accuweather"}`))
	if !errors.Is(err, ErrDataUnavailable) || !errors.Is(err, ErrProviderDisabled) {
		t.Fatalf("got %v, want unavailable disabled provider", err)
	}
}

func TestServiceAlertsMergeSources(t *testing.T) {
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/wtr-v3/weather/all"):
			_, _ = io.WriteString(writer, `{
			  "current":{"temperature":{"value":29},"weather":"晴"},
			  "forecastHourly":{"pubTime":"2026-08-27T08:00:00Z","temperature":{"value":[29]}},
			  "alerts":[{"title":"雷电黄色预警","level":"黄色"}]
			}`)
		case strings.Contains(request.URL.Path, "geoposition"):
			_, _ = io.WriteString(writer, `{"Key":"123","LocalizedName":"武昌","GeoPosition":{"Latitude":30.538,"Longitude":114.362}}`)
		case strings.Contains(request.URL.Path, "alerts"):
			_, _ = io.WriteString(writer, `[{"AlertID":9,"Description":{"Localized":"高温预警"},"Category":"Heat","Type":"Warning","Source":"CMA"}]`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	service := NewServiceWithClock(serviceTestClient(server, now, "test-key"), func() time.Time { return now })
	raw, err := service.Alerts(t.Context(), "campus-services", []byte(`{"latitude":30.4,"longitude":114.3}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded AlertsResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Alerts) != 2 {
		t.Fatalf("alerts=%+v", decoded)
	}
	if decoded.DataStatus.Source != "weather-alerts" || decoded.DataStatus.Provider != ProviderAuto {
		t.Fatalf("merged status=%+v", decoded.DataStatus)
	}
}
