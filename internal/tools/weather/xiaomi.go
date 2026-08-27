package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// 小米天气公开 HTTP 接口字段随版本变化，这里只抽取已知稳定字段，拒绝把原始响应当业务事实。
type xiaomiAllResponse struct {
	Current        json.RawMessage `json:"current"`
	ForecastHourly json.RawMessage `json:"forecastHourly"`
	AQI            json.RawMessage `json:"aqi"`
	Alerts         json.RawMessage `json:"alerts"`
}

type xiaomiCurrent struct {
	Temperature json.RawMessage `json:"temperature"`
	Humidity    json.RawMessage `json:"humidity"`
	Weather     json.RawMessage `json:"weather"`
	Wind        json.RawMessage `json:"wind"`
	PubTime     string          `json:"pubTime"`
}

type xiaomiHourly struct {
	PubTime     string          `json:"pubTime"`
	Temperature json.RawMessage `json:"temperature"`
	Weather     json.RawMessage `json:"weather"`
	AQI         json.RawMessage `json:"aqi"`
}

type xiaomiAQI struct {
	AQI     json.RawMessage `json:"aqi"`
	Suggest string          `json:"suggest"`
	Primary string          `json:"primary"`
	PubTime string          `json:"pubTime"`
}

func (c *Client) XiaomiFetch(ctx context.Context, appID string, query LocationQuery) (XiaomiResult, error) {
	if err := query.NormalizeAndValidate(); err != nil {
		return XiaomiResult{}, err
	}
	cacheKey := CacheKey(ProviderXiaomi, "fetch", query.Latitude, query.Longitude, query.Hours)
	if payload, entry, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return XiaomiResult{}, err
	} else if ok {
		var result XiaomiResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return XiaomiResult{}, fmt.Errorf("%w: cached xiaomi weather is invalid", contracts.ErrDataIncomplete)
		}
		result.DataStatus.CacheHit = true
		result.DataStatus.Revision = entry.SourceRevision
		governed, err := result.DataStatus.Govern(c.now())
		if err != nil {
			return XiaomiResult{}, err
		}
		result.DataStatus = governed
		return result, nil
	}
	values := url.Values{}
	values.Set("latitude", formatCoord(query.Latitude))
	values.Set("longitude", formatCoord(query.Longitude))
	values.Set("isGlobal", "true")
	values.Set("locale", "zh_cn")
	values.Set("days", "1")
	values.Set("appKey", c.xiaomiAppKey)
	values.Set("sign", c.xiaomiSign)
	rawURL, err := c.join(c.xiaomiBase, "/wtr-v3/weather/all", values)
	if err != nil {
		return XiaomiResult{}, err
	}
	var raw xiaomiAllResponse
	if err := c.getJSON(ctx, ProviderXiaomi, rawURL, &raw); err != nil {
		return XiaomiResult{}, err
	}
	result, err := normalizeXiaomi(query, raw, c.now())
	if err != nil {
		return XiaomiResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return XiaomiResult{}, fmt.Errorf("encode xiaomi weather: %w", err)
	}
	entry, err := c.store(ctx, appID, cacheKey, ProviderXiaomi, result.DataStatus.Revision, encoded, HourlyTTL)
	if err != nil {
		return XiaomiResult{}, err
	}
	result.DataStatus.Revision = entry.SourceRevision
	observe.Info(ctx, "小米天气已规范化",
		observe.StringAttr("provider", ProviderXiaomi),
		observe.IntAttr("hourly_count", len(result.Hourly)),
		observe.IntAttr("alert_count", len(result.Alerts)),
		observe.BoolAttr("cache_hit", false),
	)
	return result, nil
}

func normalizeXiaomi(query LocationQuery, raw xiaomiAllResponse, now time.Time) (XiaomiResult, error) {
	if len(raw.Current) == 0 {
		return XiaomiResult{}, fmt.Errorf("%w: xiaomi current weather is incomplete", contracts.ErrDataIncomplete)
	}
	var currentRaw xiaomiCurrent
	if err := json.Unmarshal(raw.Current, &currentRaw); err != nil {
		return XiaomiResult{}, fmt.Errorf("%w: xiaomi current weather is invalid", contracts.ErrDataIncomplete)
	}
	temp, ok := extractFloat(currentRaw.Temperature)
	if !ok {
		return XiaomiResult{}, fmt.Errorf("%w: xiaomi temperature is incomplete", contracts.ErrDataIncomplete)
	}
	observedAt := now.UTC()
	if parsed, err := parseRFC3339(currentRaw.PubTime); err == nil {
		observedAt = parsed
	}
	weatherText := extractString(currentRaw.Weather)
	if weatherText == "" {
		weatherText = "未知天气"
	}
	current := Current{
		ObservedAt:   observedAt,
		Timezone:     DefaultTimezone,
		TemperatureC: temp,
		WeatherText:  weatherText,
		WeatherCode:  xiaomiWeatherCode(weatherText),
	}
	if humidity, ok := extractInt(currentRaw.Humidity); ok {
		current.HumidityPercent = &humidity
	}
	hourly, err := normalizeXiaomiHourly(raw.ForecastHourly, now, query.Hours)
	if err != nil {
		return XiaomiResult{}, err
	}
	fetchedAt := now.UTC()
	result := XiaomiResult{
		DataStatus: NewDataStatus(ProviderXiaomi, "xiaomi-weather", revision(ProviderXiaomi, fetchedAt), fetchedAt, fetchedAt.Add(HourlyTTL), false),
		Location:   locationFromQuery(query, Location{Timezone: DefaultTimezone}),
		Current:    current,
		Hourly:     hourly,
		Alerts:     normalizeXiaomiAlerts(raw.Alerts),
	}
	if aqi, ok := normalizeXiaomiAQI(raw.AQI, observedAt); ok {
		result.AQI = &aqi
	}
	return result, nil
}

func normalizeXiaomiHourly(raw json.RawMessage, now time.Time, hours int) ([]HourlyPoint, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: xiaomi hourly weather is incomplete", contracts.ErrDataIncomplete)
	}
	var hourly xiaomiHourly
	if err := json.Unmarshal(raw, &hourly); err != nil {
		return nil, fmt.Errorf("%w: xiaomi hourly weather is invalid", contracts.ErrDataIncomplete)
	}
	temps := extractFloatSlice(hourly.Temperature)
	weathers := extractStringSlice(hourly.Weather)
	if len(temps) == 0 {
		return nil, fmt.Errorf("%w: xiaomi hourly weather is incomplete", contracts.ErrDataIncomplete)
	}
	start := now.UTC().Truncate(time.Hour)
	if parsed, err := parseRFC3339(hourly.PubTime); err == nil {
		start = parsed.UTC().Truncate(time.Hour)
	}
	limit := hours
	if limit > len(temps) {
		limit = len(temps)
	}
	points := make([]HourlyPoint, 0, limit)
	for i := 0; i < limit; i++ {
		text := "未知天气"
		if i < len(weathers) && weathers[i] != "" {
			text = weathers[i]
		}
		points = append(points, HourlyPoint{
			Time:         start.Add(time.Duration(i) * time.Hour),
			TemperatureC: temps[i],
			WeatherText:  text,
			WeatherCode:  xiaomiWeatherCode(text),
		})
	}
	return points, nil
}

func normalizeXiaomiAQI(raw json.RawMessage, observedAt time.Time) (AQI, bool) {
	if len(raw) == 0 {
		return AQI{}, false
	}
	var parsed xiaomiAQI
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return AQI{}, false
	}
	index, ok := extractInt(parsed.AQI)
	if !ok {
		return AQI{}, false
	}
	if parsed.PubTime != "" {
		if t, err := parseRFC3339(parsed.PubTime); err == nil {
			observedAt = t
		}
	}
	return AQI{
		ObservedAt:        observedAt,
		Index:             index,
		Scale:             "china-mee",
		Category:          ChinaAQICategory(index),
		DominantPollutant: parsed.Primary,
	}, true
}

func normalizeXiaomiAlerts(raw json.RawMessage) []Alert {
	if len(raw) == 0 || string(raw) == "null" {
		return []Alert{}
	}
	var direct []map[string]any
	if err := json.Unmarshal(raw, &direct); err == nil {
		return mapXiaomiAlerts(direct)
	}
	var wrapped struct {
		Alerts []map[string]any `json:"alerts"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return []Alert{}
	}
	return mapXiaomiAlerts(wrapped.Alerts)
}

func mapXiaomiAlerts(items []map[string]any) []Alert {
	alerts := make([]Alert, 0, len(items))
	for i, item := range items {
		headline := stringField(item, "title", "headline", "name")
		if headline == "" {
			continue
		}
		id := stringField(item, "alertId", "id")
		if id == "" {
			id = fmt.Sprintf("xiaomi-%d", i+1)
		}
		alert := Alert{
			ID:          id,
			Headline:    headline,
			Severity:    alertSeverity(stringField(item, "level", "severity", "type")),
			Event:       stringField(item, "type", "event"),
			Description: stringField(item, "detail", "description", "text"),
			Source:      "xiaomi-weather",
		}
		if onset, err := parseRFC3339(stringField(item, "pubTime", "startTime", "onset")); err == nil {
			alert.Onset = &onset
		}
		alerts = append(alerts, alert)
	}
	return alerts
}

func extractFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var direct float64
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(asString), 64)
		return parsed, err == nil
	}
	var wrapped struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return 0, false
	}
	return extractFloat(wrapped.Value)
}

func extractInt(raw json.RawMessage) (int, bool) {
	value, ok := extractFloat(raw)
	if !ok {
		return 0, false
	}
	return int(value), true
}

func extractString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return strings.TrimSpace(direct)
	}
	var wrapped struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		return strings.TrimSpace(wrapped.Value)
	}
	return ""
}

func extractFloatSlice(raw json.RawMessage) []float64 {
	var wrapped struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Value) > 0 {
		raw = wrapped.Value
	}
	var values []float64
	if err := json.Unmarshal(raw, &values); err == nil {
		return values
	}
	var asStrings []string
	if err := json.Unmarshal(raw, &asStrings); err != nil {
		return nil
	}
	out := make([]float64, 0, len(asStrings))
	for _, item := range asStrings {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(item), 64)
		if err != nil {
			return nil
		}
		out = append(out, parsed)
	}
	return out
}

func extractStringSlice(raw json.RawMessage) []string {
	var wrapped struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Value) > 0 {
		raw = wrapped.Value
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		return values
	}
	return nil
}

func stringField(item map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case float64:
			return strconv.FormatFloat(typed, 'f', -1, 64)
		}
	}
	return ""
}

func xiaomiWeatherCode(text string) int {
	switch {
	case strings.Contains(text, "雷"):
		return 95
	case strings.Contains(text, "雪"):
		return 71
	case strings.Contains(text, "雨"):
		return 63
	case strings.Contains(text, "雾"):
		return 45
	case strings.Contains(text, "阴"):
		return 3
	case strings.Contains(text, "云"):
		return 2
	case strings.Contains(text, "晴"):
		return 0
	default:
		return 1
	}
}
