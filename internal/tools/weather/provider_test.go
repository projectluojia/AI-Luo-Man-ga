package weather

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

func TestOpenMeteoNormalizesForecastAndAQI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "air-quality"):
			_, _ = io.WriteString(writer, openMeteoAirJSON)
		default:
			_, _ = io.WriteString(writer, openMeteoForecastJSON)
		}
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
	result, err := normalizeOpenMeteoAir(query, openMeteoAirResponse{
		Current: struct {
			Time        string   `json:"time"`
			USAQI       *int     `json:"us_aqi"`
			EuropeanAQI *int     `json:"european_aqi"`
			PM25        *float64 `json:"pm2_5"`
			PM10        *float64 `json:"pm10"`
		}{EuropeanAQI: func() *int { value := 30; return &value }()},
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
	if _, err := client.OpenMeteoForecast(t.Context(), "campus-services", LocationQuery{Latitude: 1, Longitude: 2, Hours: 1}); !errors.Is(err, contracts.ErrDataIncomplete) {
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
	if !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("got %v, want schema validation", err)
	}
}
