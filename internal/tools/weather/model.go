// Package weather 提供跨 Service 可复用的天气原子 Tool：直连 Open-Meteo、
// 小米天气与 AccuWeather，规范化逐小时/AQI/预警，并带缓存与有界重试。
package weather

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	ServiceID = "weather"

	OpenMeteoForecastToolID   = "weather.openmeteo.forecast"
	OpenMeteoAirQualityToolID = "weather.openmeteo.airquality"
	OpenMeteoGeocodeToolID    = "weather.openmeteo.geocode"
	XiaomiFetchToolID         = "weather.xiaomi.fetch"
	AccuWeatherFetchToolID    = "weather.accuweather.fetch"
	AccuWeatherAlertsToolID   = "weather.accuweather.alerts"

	ProviderOpenMeteo   = "openmeteo"
	ProviderXiaomi      = "xiaomi"
	ProviderAccuWeather = "accuweather"
	ProviderAuto        = "auto"

	DataStateAuthoritativeFresh = "authoritative_fresh"

	DefaultPlace     = "武汉大学珞珈山"
	DefaultLatitude  = 30.5383
	DefaultLongitude = 114.3617
	DefaultTimezone  = "Asia/Shanghai"
	DefaultHours     = 24
	MaxHours         = 48
	MaxPlaceRunes    = 128

	CurrentTTL = 10 * time.Minute
	HourlyTTL  = 30 * time.Minute
	AQITTL     = 30 * time.Minute
	AlertsTTL  = 10 * time.Minute
	GeocodeTTL = 24 * time.Hour
)

var (
	ErrInvalidRequest   = errors.New("weather request is invalid")
	ErrPlaceNotFound    = errors.New("weather place was not found")
	ErrProviderDisabled = errors.New("weather provider is not configured")
)

// LocationInputSchemaJSON 是按坐标查询的 Tool 输入契约。
const LocationInputSchemaJSON = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false,
  "required":["latitude","longitude"],
  "properties":{
    "latitude":{"type":"number","minimum":-90,"maximum":90},
    "longitude":{"type":"number","minimum":-180,"maximum":180},
    "hours":{"type":"integer","minimum":1,"maximum":48}
  }
}`

// GeocodeInputSchemaJSON 是地名解析 Tool 输入契约。
const GeocodeInputSchemaJSON = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false,
  "required":["place"],
  "properties":{
    "place":{"type":"string","minLength":1,"maxLength":128}
  }
}`

// LookupInputSchemaJSON 是对外 Capability 输入契约：地名、坐标、时长与数据源均可选。
const LookupInputSchemaJSON = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "place":{"type":"string","minLength":1,"maxLength":128},
    "latitude":{"type":"number","minimum":-90,"maximum":90},
    "longitude":{"type":"number","minimum":-180,"maximum":180},
    "hours":{"type":"integer","minimum":1,"maximum":48},
    "source":{"type":"string","enum":["auto","openmeteo","xiaomi","accuweather"]}
  },
  "dependentRequired":{
    "latitude":["longitude"],
    "longitude":["latitude"]
  }
}`

type LocationQuery struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Hours     int     `json:"hours,omitempty"`
}

func (q *LocationQuery) NormalizeAndValidate() error {
	if q.Hours == 0 {
		q.Hours = DefaultHours
	}
	if q.Hours < 1 || q.Hours > MaxHours {
		return fmt.Errorf("%w: hours must be between 1 and %d", ErrInvalidRequest, MaxHours)
	}
	if err := validateCoordinates(q.Latitude, q.Longitude); err != nil {
		return err
	}
	q.Latitude = roundCoord(q.Latitude)
	q.Longitude = roundCoord(q.Longitude)
	return nil
}

type GeocodeQuery struct {
	Place string `json:"place"`
}

func (q *GeocodeQuery) NormalizeAndValidate() error {
	q.Place = strings.TrimSpace(q.Place)
	if q.Place == "" || utf8.RuneCountInString(q.Place) > MaxPlaceRunes {
		return fmt.Errorf("%w: place is required and must be at most %d characters", ErrInvalidRequest, MaxPlaceRunes)
	}
	return nil
}

type LookupRequest struct {
	Place     string   `json:"place,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Hours     int      `json:"hours,omitempty"`
	Source    string   `json:"source,omitempty"`
}

func (r *LookupRequest) NormalizeAndValidate() error {
	r.Place = strings.TrimSpace(r.Place)
	r.Source = strings.TrimSpace(strings.ToLower(r.Source))
	if r.Source == "" {
		r.Source = ProviderAuto
	}
	switch r.Source {
	case ProviderAuto, ProviderOpenMeteo, ProviderXiaomi, ProviderAccuWeather:
	default:
		return fmt.Errorf("%w: unknown weather source %q", ErrInvalidRequest, r.Source)
	}
	if r.Hours == 0 {
		r.Hours = DefaultHours
	}
	if r.Hours < 1 || r.Hours > MaxHours {
		return fmt.Errorf("%w: hours must be between 1 and %d", ErrInvalidRequest, MaxHours)
	}
	if (r.Latitude == nil) != (r.Longitude == nil) {
		return fmt.Errorf("%w: latitude and longitude must be provided together", ErrInvalidRequest)
	}
	if r.Latitude != nil {
		if err := validateCoordinates(*r.Latitude, *r.Longitude); err != nil {
			return err
		}
		lat := roundCoord(*r.Latitude)
		lon := roundCoord(*r.Longitude)
		r.Latitude = &lat
		r.Longitude = &lon
	}
	if r.Place != "" && utf8.RuneCountInString(r.Place) > MaxPlaceRunes {
		return fmt.Errorf("%w: place must be at most %d characters", ErrInvalidRequest, MaxPlaceRunes)
	}
	if r.Latitude == nil && r.Place == "" {
		r.Place = DefaultPlace
		lat := DefaultLatitude
		lon := DefaultLongitude
		r.Latitude = &lat
		r.Longitude = &lon
	}
	return nil
}

func (r LookupRequest) LocationQuery() LocationQuery {
	query := LocationQuery{Hours: r.Hours}
	if r.Latitude != nil && r.Longitude != nil {
		query.Latitude = *r.Latitude
		query.Longitude = *r.Longitude
	}
	return query
}

type Location struct {
	Place     string  `json:"place"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone,omitempty"`
	Country   string  `json:"country,omitempty"`
	Admin1    string  `json:"admin1,omitempty"`
}

type DataStatus struct {
	State         string    `json:"state"`
	Source        string    `json:"source"`
	Provider      string    `json:"provider"`
	Revision      string    `json:"source_revision"`
	Authoritative bool      `json:"authoritative"`
	Complete      bool      `json:"complete"`
	FetchedAt     time.Time `json:"fetched_at"`
	ValidUntil    time.Time `json:"valid_until"`
	CacheHit      bool      `json:"cache_hit"`
}

func (s DataStatus) Govern(now time.Time) (DataStatus, error) {
	if s.Source == "" || s.Provider == "" || s.Revision == "" || s.FetchedAt.IsZero() || s.ValidUntil.IsZero() ||
		!s.ValidUntil.After(s.FetchedAt) || now.Before(s.FetchedAt.Add(-time.Minute)) {
		return DataStatus{}, contracts.ErrDataIncomplete
	}
	if !s.Authoritative || !s.Complete {
		return DataStatus{}, contracts.ErrDataUntrusted
	}
	if !now.Before(s.ValidUntil) {
		return DataStatus{}, contracts.ErrDataExpired
	}
	s.State = DataStateAuthoritativeFresh
	return s, nil
}

func NewDataStatus(provider, source, revision string, fetchedAt, validUntil time.Time, cacheHit bool) DataStatus {
	return DataStatus{
		State:         DataStateAuthoritativeFresh,
		Source:        source,
		Provider:      provider,
		Revision:      revision,
		Authoritative: true,
		Complete:      true,
		FetchedAt:     fetchedAt.UTC(),
		ValidUntil:    validUntil.UTC(),
		CacheHit:      cacheHit,
	}
}

type Current struct {
	ObservedAt           time.Time `json:"observed_at"`
	Timezone             string    `json:"timezone,omitempty"`
	TemperatureC         float64   `json:"temperature_c"`
	ApparentTemperatureC *float64  `json:"apparent_temperature_c,omitempty"`
	HumidityPercent      *int      `json:"humidity_percent,omitempty"`
	WeatherCode          int       `json:"weather_code"`
	WeatherText          string    `json:"weather_text"`
	WindSpeedMps         *float64  `json:"wind_speed_mps,omitempty"`
	WindDirectionDeg     *int      `json:"wind_direction_deg,omitempty"`
	PrecipitationMM      *float64  `json:"precipitation_mm,omitempty"`
	VisibilityM          *float64  `json:"visibility_m,omitempty"`
}

type HourlyPoint struct {
	Time                     time.Time `json:"time"`
	TemperatureC             float64   `json:"temperature_c"`
	HumidityPercent          *int      `json:"humidity_percent,omitempty"`
	WeatherCode              int       `json:"weather_code"`
	WeatherText              string    `json:"weather_text"`
	PrecipitationMM          *float64  `json:"precipitation_mm,omitempty"`
	PrecipitationProbability *int      `json:"precipitation_probability,omitempty"`
	WindSpeedMps             *float64  `json:"wind_speed_mps,omitempty"`
}

type AQI struct {
	ObservedAt        time.Time `json:"observed_at"`
	Index             int       `json:"index"`
	Scale             string    `json:"scale"`
	Category          string    `json:"category"`
	DominantPollutant string    `json:"dominant_pollutant,omitempty"`
	PM25              *float64  `json:"pm2_5,omitempty"`
	PM10              *float64  `json:"pm10,omitempty"`
}

type Alert struct {
	ID          string     `json:"id"`
	Headline    string     `json:"headline"`
	Severity    string     `json:"severity"`
	Event       string     `json:"event,omitempty"`
	Onset       *time.Time `json:"onset,omitempty"`
	Ends        *time.Time `json:"ends,omitempty"`
	Description string     `json:"description,omitempty"`
	Source      string     `json:"source,omitempty"`
}

type ForecastResult struct {
	DataStatus DataStatus    `json:"data_status"`
	Location   Location      `json:"location"`
	Current    Current       `json:"current"`
	Hourly     []HourlyPoint `json:"hourly"`
}

type AirQualityResult struct {
	DataStatus DataStatus `json:"data_status"`
	Location   Location   `json:"location"`
	AQI        AQI        `json:"aqi"`
}

type AlertsResult struct {
	DataStatus DataStatus `json:"data_status"`
	Location   Location   `json:"location"`
	Alerts     []Alert    `json:"alerts"`
}

type XiaomiResult struct {
	DataStatus DataStatus    `json:"data_status"`
	Location   Location      `json:"location"`
	Current    Current       `json:"current"`
	Hourly     []HourlyPoint `json:"hourly"`
	AQI        *AQI          `json:"aqi,omitempty"`
	Alerts     []Alert       `json:"alerts"`
}

type GeocodeResult struct {
	DataStatus DataStatus `json:"data_status"`
	Location   Location   `json:"location"`
}

type CurrentView struct {
	DataStatus DataStatus `json:"data_status"`
	Location   Location   `json:"location"`
	Current    Current    `json:"current"`
}

type HourlyView struct {
	DataStatus DataStatus    `json:"data_status"`
	Location   Location      `json:"location"`
	Hourly     []HourlyPoint `json:"hourly"`
}

func decodeLookup(payload json.RawMessage) (LookupRequest, error) {
	var request LookupRequest
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := jsonutil.DecodeStrict(payload, &request); err != nil {
		return LookupRequest{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	if err := request.NormalizeAndValidate(); err != nil {
		return LookupRequest{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	return request, nil
}

func decodeLocation(payload json.RawMessage) (LocationQuery, error) {
	var query LocationQuery
	if err := jsonutil.DecodeStrict(payload, &query); err != nil {
		return LocationQuery{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	if err := query.NormalizeAndValidate(); err != nil {
		return LocationQuery{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	return query, nil
}

func decodeGeocode(payload json.RawMessage) (GeocodeQuery, error) {
	var query GeocodeQuery
	if err := jsonutil.DecodeStrict(payload, &query); err != nil {
		return GeocodeQuery{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	if err := query.NormalizeAndValidate(); err != nil {
		return GeocodeQuery{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	return query, nil
}

func validateCoordinates(lat, lon float64) error {
	if math.IsNaN(lat) || math.IsInf(lat, 0) || lat < -90 || lat > 90 {
		return fmt.Errorf("%w: latitude is out of range", ErrInvalidRequest)
	}
	if math.IsNaN(lon) || math.IsInf(lon, 0) || lon < -180 || lon > 180 {
		return fmt.Errorf("%w: longitude is out of range", ErrInvalidRequest)
	}
	return nil
}

func roundCoord(value float64) float64 {
	return math.Round(value*1000) / 1000
}

func marshalResult(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode weather result: %w", err)
	}
	return encoded, nil
}

func defaultLocation() Location {
	return Location{
		Place:     DefaultPlace,
		Latitude:  DefaultLatitude,
		Longitude: DefaultLongitude,
		Timezone:  DefaultTimezone,
		Country:   "中国",
		Admin1:    "湖北省",
	}
}

func locationFromQuery(query LocationQuery, base Location) Location {
	if base.Place == "" {
		base.Place = DefaultPlace
	}
	base.Latitude = query.Latitude
	base.Longitude = query.Longitude
	if base.Timezone == "" {
		base.Timezone = DefaultTimezone
	}
	return base
}

// WeatherText 把 WMO 天气代码映射为中文简述；未知代码保持“未知天气”而不编造。
func WeatherText(code int) string {
	if text, ok := wmoWeatherText[code]; ok {
		return text
	}
	return "未知天气"
}

var wmoWeatherText = map[int]string{
	0: "晴", 1: "大部晴朗", 2: "多云", 3: "阴",
	45: "雾", 48: "沉积雾凇",
	51: "小毛毛雨", 53: "中毛毛雨", 55: "大毛毛雨",
	56: "轻度冻毛毛雨", 57: "冻毛毛雨",
	61: "小雨", 63: "中雨", 65: "大雨",
	66: "轻度冻雨", 67: "冻雨",
	71: "小雪", 73: "中雪", 75: "大雪",
	77: "雪粒",
	80: "小阵雨", 81: "中阵雨", 82: "强阵雨",
	85: "小阵雪", 86: "强阵雪",
	95: "雷暴", 96: "雷暴伴小冰雹", 99: "雷暴伴大冰雹",
}

func USAqiCategory(index int) string {
	switch {
	case index <= 50:
		return "优"
	case index <= 100:
		return "中等"
	case index <= 150:
		return "对敏感人群不健康"
	case index <= 200:
		return "不健康"
	case index <= 300:
		return "非常不健康"
	default:
		return "危险"
	}
}

func ChinaAQICategory(index int) string {
	switch {
	case index <= 50:
		return "优"
	case index <= 100:
		return "良"
	case index <= 150:
		return "轻度污染"
	case index <= 200:
		return "中度污染"
	case index <= 300:
		return "重度污染"
	default:
		return "严重污染"
	}
}

func alertSeverity(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(normalized, "warning") || strings.Contains(normalized, "红色") || strings.Contains(normalized, "橙色"):
		return "warning"
	case strings.Contains(normalized, "watch") || strings.Contains(normalized, "黄色"):
		return "watch"
	case strings.Contains(normalized, "advisory") || strings.Contains(normalized, "蓝色") || strings.Contains(normalized, "白色"):
		return "advisory"
	default:
		if normalized == "" {
			return "unknown"
		}
		return normalized
	}
}

func parseRFC3339(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, contracts.ErrDataIncomplete
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, contracts.ErrDataIncomplete
}
