package weather_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	weatherservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/weather"
	wx "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/weather"
)

func TestWeatherCapabilitiesUseCampusDefaultAndFallback(t *testing.T) {
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
	dispatcher := newWeatherDispatcher(t, server, now, "")

	current, err := invoke(t, dispatcher, weatherservice.CurrentCapabilityID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if current["current"].(map[string]any)["temperature_c"].(float64) != 31.2 {
		t.Fatalf("current=%v", current)
	}
	if current["location"].(map[string]any)["place"] != wx.DefaultPlace {
		t.Fatalf("default place=%v", current["location"])
	}

	named, err := invoke(t, dispatcher, weatherservice.HourlyCapabilityID, `{"place":"信息学部","hours":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if named["location"].(map[string]any)["place"] != "信息学部" {
		t.Fatalf("named location=%v", named["location"])
	}

	aqi, err := invoke(t, dispatcher, weatherservice.AQICapabilityID, `{}`)
	if err != nil || aqi["aqi"].(map[string]any)["index"].(float64) != 40 {
		t.Fatalf("aqi=%v err=%v", aqi, err)
	}

	alerts, err := invoke(t, dispatcher, weatherservice.AlertsCapabilityID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	list := alerts["alerts"].([]any)
	if len(list) != 1 {
		t.Fatalf("alerts=%v", alerts)
	}

	openMeteoDown = true
	fallback, err := invoke(t, dispatcher, weatherservice.CurrentCapabilityID, `{"latitude":30.4,"longitude":114.3,"source":"auto"}`)
	if err != nil {
		t.Fatal(err)
	}
	if fallback["data_status"].(map[string]any)["provider"] != wx.ProviderXiaomi {
		t.Fatalf("fallback provider=%v", fallback["data_status"])
	}

	_, err = invoke(t, dispatcher, weatherservice.CurrentCapabilityID, `{"unknown":1}`)
	if !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("got %v, want schema validation", err)
	}
	_, err = invoke(t, dispatcher, weatherservice.AlertsCapabilityID, `{"source":"openmeteo"}`)
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("openmeteo alerts got %v, want unavailable", err)
	}
}

func TestWeatherAccuWeatherDisabledIsSkipped(t *testing.T) {
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
	dispatcher := newWeatherDispatcher(t, server, now, "")
	_, err := invoke(t, dispatcher, weatherservice.CurrentCapabilityID, `{"source":"accuweather"}`)
	if !errors.Is(err, contracts.ErrDataUnavailable) || !errors.Is(err, wx.ErrProviderDisabled) {
		t.Fatalf("got %v, want unavailable disabled provider", err)
	}
}

func newWeatherDispatcher(t *testing.T, server *httptest.Server, now time.Time, accuKey string) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	client := wx.NewClient(wx.ClientConfig{
		HTTP: server.Client(), Cache: wx.NewMemoryCache(), AllowHTTP: true,
		OpenMeteoBase: server.URL, OpenMeteoAirBase: server.URL, OpenMeteoGeoBase: server.URL,
		XiaomiBase: server.URL, AccuWeatherBase: server.URL, AccuWeatherAPIKey: accuKey,
		Now: func() time.Time { return now }, MaxRetries: 0, RequestsPerMinute: 600,
	})
	if err := wx.RegisterTools(reg, client); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	for _, id := range weatherservice.CapabilityIDs() {
		policy.Enable("campus-services", id)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	if err := weatherservice.Register(reg, weatherservice.NewService(dispatcher, client)); err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func invoke(t *testing.T, dispatcher *runtime.Dispatcher, capabilityID, payload string) (map[string]any, error) {
	t.Helper()
	raw, err := dispatcher.InvokeCapability(t.Context(), contracts.RequestContext{
		AppID: "campus-services", EchoID: "echo", RequestID: "req", Deadline: time.Now().Add(time.Minute),
	}, capabilityID, json.RawMessage(payload))
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, nil
}
