package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type accuLocation struct {
	Key           string `json:"Key"`
	LocalizedName string `json:"LocalizedName"`
	TimeZone      struct {
		Name string `json:"Name"`
	} `json:"TimeZone"`
	Country struct {
		LocalizedName string `json:"LocalizedName"`
	} `json:"Country"`
	AdministrativeArea struct {
		LocalizedName string `json:"LocalizedName"`
	} `json:"AdministrativeArea"`
	GeoPosition struct {
		Latitude  float64 `json:"Latitude"`
		Longitude float64 `json:"Longitude"`
	} `json:"GeoPosition"`
}

type accuCurrent struct {
	LocalObservationDateTime string `json:"LocalObservationDateTime"`
	WeatherText              string `json:"WeatherText"`
	WeatherIcon              int    `json:"WeatherIcon"`
	RelativeHumidity         *int   `json:"RelativeHumidity"`
	Temperature              struct {
		Metric struct {
			Value *float64 `json:"Value"`
		} `json:"Metric"`
	} `json:"Temperature"`
	RealFeelTemperature struct {
		Metric struct {
			Value *float64 `json:"Value"`
		} `json:"Metric"`
	} `json:"RealFeelTemperature"`
	Wind struct {
		Direction struct {
			Degrees *int `json:"Degrees"`
		} `json:"Direction"`
		Speed struct {
			Metric struct {
				Value *float64 `json:"Value"`
				Unit  string   `json:"Unit"`
			} `json:"Metric"`
		} `json:"Speed"`
	} `json:"Wind"`
	PrecipitationSummary struct {
		Precipitation struct {
			Metric struct {
				Value *float64 `json:"Value"`
			} `json:"Metric"`
		} `json:"Precipitation"`
	} `json:"PrecipitationSummary"`
	Visibility struct {
		Metric struct {
			Value *float64 `json:"Value"`
			Unit  string   `json:"Unit"`
		} `json:"Metric"`
	} `json:"Visibility"`
}

type accuHourly struct {
	DateTime    string `json:"DateTime"`
	WeatherIcon int    `json:"WeatherIcon"`
	IconPhrase  string `json:"IconPhrase"`
	Temperature struct {
		Value *float64 `json:"Value"`
	} `json:"Temperature"`
	RelativeHumidity         *int `json:"RelativeHumidity"`
	PrecipitationProbability *int `json:"PrecipitationProbability"`
	Rain                     struct {
		Value *float64 `json:"Value"`
	} `json:"Rain"`
	Wind struct {
		Speed struct {
			Value *float64 `json:"Value"`
			Unit  string   `json:"Unit"`
		} `json:"Speed"`
	} `json:"Wind"`
}

type accuAlert struct {
	AlertID     json.RawMessage `json:"AlertID"`
	Description struct {
		Localized string `json:"Localized"`
	} `json:"Description"`
	Category string          `json:"Category"`
	Type     string          `json:"Type"`
	Severity json.RawMessage `json:"Severity"`
	Source   string          `json:"Source"`
	Area     []struct {
		StartTime string `json:"StartTime"`
		EndTime   string `json:"EndTime"`
	} `json:"Area"`
}

func (c *Client) AccuWeatherFetch(ctx context.Context, appID string, query LocationQuery) (ForecastResult, error) {
	if c.accuWeatherAPIKey == "" {
		return ForecastResult{}, fmt.Errorf("%w: accuweather", ErrProviderDisabled)
	}
	if err := query.NormalizeAndValidate(); err != nil {
		return ForecastResult{}, err
	}
	cacheKey := CacheKey(ProviderAccuWeather, "fetch", query.Latitude, query.Longitude, query.Hours)
	if payload, entry, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return ForecastResult{}, err
	} else if ok {
		var result ForecastResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return ForecastResult{}, fmt.Errorf("%w: cached accuweather forecast is invalid", contracts.ErrDataIncomplete)
		}
		result.DataStatus.CacheHit = true
		result.DataStatus.Revision = entry.SourceRevision
		governed, err := result.DataStatus.Govern(c.now())
		if err != nil {
			return ForecastResult{}, err
		}
		result.DataStatus = governed
		return result, nil
	}
	location, err := c.accuLocation(ctx, appID, query)
	if err != nil {
		return ForecastResult{}, err
	}
	current, err := c.accuCurrent(ctx, location.Key)
	if err != nil {
		return ForecastResult{}, err
	}
	hourly, err := c.accuHourly(ctx, location.Key, query.Hours)
	if err != nil {
		return ForecastResult{}, err
	}
	fetchedAt := c.now().UTC()
	result := ForecastResult{
		DataStatus: NewDataStatus(ProviderAccuWeather, "accuweather-forecast", revision(ProviderAccuWeather, fetchedAt), fetchedAt, fetchedAt.Add(HourlyTTL), false),
		Location: Location{
			Place:     firstNonEmpty(location.LocalizedName, DefaultPlace),
			Latitude:  roundCoord(location.GeoPosition.Latitude),
			Longitude: roundCoord(location.GeoPosition.Longitude),
			Timezone:  firstNonEmpty(location.TimeZone.Name, DefaultTimezone),
			Country:   location.Country.LocalizedName,
			Admin1:    location.AdministrativeArea.LocalizedName,
		},
		Current: current,
		Hourly:  hourly,
	}
	if result.Location.Latitude == 0 && result.Location.Longitude == 0 {
		result.Location = locationFromQuery(query, result.Location)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ForecastResult{}, fmt.Errorf("encode accuweather forecast: %w", err)
	}
	entry, err := c.store(ctx, appID, cacheKey, ProviderAccuWeather, result.DataStatus.Revision, encoded, HourlyTTL)
	if err != nil {
		return ForecastResult{}, err
	}
	result.DataStatus.Revision = entry.SourceRevision
	observe.Info(ctx, "AccuWeather 预报已规范化",
		observe.StringAttr("provider", ProviderAccuWeather),
		observe.IntAttr("hourly_count", len(result.Hourly)),
		observe.BoolAttr("cache_hit", false),
	)
	return result, nil
}

func (c *Client) AccuWeatherAlerts(ctx context.Context, appID string, query LocationQuery) (AlertsResult, error) {
	if c.accuWeatherAPIKey == "" {
		return AlertsResult{}, fmt.Errorf("%w: accuweather", ErrProviderDisabled)
	}
	if err := query.NormalizeAndValidate(); err != nil {
		return AlertsResult{}, err
	}
	cacheKey := CacheKey(ProviderAccuWeather, "alerts", query.Latitude, query.Longitude)
	if payload, entry, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return AlertsResult{}, err
	} else if ok {
		var result AlertsResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return AlertsResult{}, fmt.Errorf("%w: cached accuweather alerts are invalid", contracts.ErrDataIncomplete)
		}
		result.DataStatus.CacheHit = true
		result.DataStatus.Revision = entry.SourceRevision
		governed, err := result.DataStatus.Govern(c.now())
		if err != nil {
			return AlertsResult{}, err
		}
		result.DataStatus = governed
		return result, nil
	}
	location, err := c.accuLocation(ctx, appID, query)
	if err != nil {
		return AlertsResult{}, err
	}
	values := url.Values{}
	values.Set("apikey", c.accuWeatherAPIKey)
	values.Set("language", "zh-cn")
	rawURL, err := c.join(c.accuWeatherBase, "/alerts/v1/"+url.PathEscape(location.Key), values)
	if err != nil {
		return AlertsResult{}, err
	}
	var raw []accuAlert
	if err := c.getJSON(ctx, ProviderAccuWeather, rawURL, &raw); err != nil {
		return AlertsResult{}, err
	}
	fetchedAt := c.now().UTC()
	result := AlertsResult{
		DataStatus: NewDataStatus(ProviderAccuWeather, "accuweather-alerts", revision(ProviderAccuWeather, fetchedAt), fetchedAt, fetchedAt.Add(AlertsTTL), false),
		Location: Location{
			Place:     firstNonEmpty(location.LocalizedName, DefaultPlace),
			Latitude:  roundCoord(location.GeoPosition.Latitude),
			Longitude: roundCoord(location.GeoPosition.Longitude),
			Timezone:  firstNonEmpty(location.TimeZone.Name, DefaultTimezone),
			Country:   location.Country.LocalizedName,
			Admin1:    location.AdministrativeArea.LocalizedName,
		},
		Alerts: normalizeAccuAlerts(raw),
	}
	if result.Location.Latitude == 0 && result.Location.Longitude == 0 {
		result.Location = locationFromQuery(query, result.Location)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return AlertsResult{}, fmt.Errorf("encode accuweather alerts: %w", err)
	}
	entry, err := c.store(ctx, appID, cacheKey, ProviderAccuWeather, result.DataStatus.Revision, encoded, AlertsTTL)
	if err != nil {
		return AlertsResult{}, err
	}
	result.DataStatus.Revision = entry.SourceRevision
	observe.Info(ctx, "AccuWeather 预警已规范化",
		observe.StringAttr("provider", ProviderAccuWeather),
		observe.IntAttr("alert_count", len(result.Alerts)),
		observe.BoolAttr("cache_hit", false),
	)
	return result, nil
}

func (c *Client) accuLocation(ctx context.Context, appID string, query LocationQuery) (accuLocation, error) {
	cacheKey := CacheKey(ProviderAccuWeather, "location", query.Latitude, query.Longitude)
	if payload, _, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return accuLocation{}, err
	} else if ok {
		var location accuLocation
		if err := json.Unmarshal(payload, &location); err != nil || location.Key == "" {
			return accuLocation{}, fmt.Errorf("%w: cached accuweather location is invalid", contracts.ErrDataIncomplete)
		}
		return location, nil
	}
	values := url.Values{}
	values.Set("apikey", c.accuWeatherAPIKey)
	values.Set("q", formatCoord(query.Latitude)+","+formatCoord(query.Longitude))
	values.Set("language", "zh-cn")
	rawURL, err := c.join(c.accuWeatherBase, "/locations/v1/cities/geoposition/search", values)
	if err != nil {
		return accuLocation{}, err
	}
	var location accuLocation
	if err := c.getJSON(ctx, ProviderAccuWeather, rawURL, &location); err != nil {
		return accuLocation{}, err
	}
	if location.Key == "" {
		return accuLocation{}, fmt.Errorf("%w: accuweather location key is missing", contracts.ErrDataIncomplete)
	}
	encoded, err := json.Marshal(location)
	if err != nil {
		return accuLocation{}, fmt.Errorf("encode accuweather location: %w", err)
	}
	if _, err := c.store(ctx, appID, cacheKey, ProviderAccuWeather, location.Key, encoded, GeocodeTTL); err != nil {
		return accuLocation{}, err
	}
	return location, nil
}

func (c *Client) accuCurrent(ctx context.Context, locationKey string) (Current, error) {
	values := url.Values{}
	values.Set("apikey", c.accuWeatherAPIKey)
	values.Set("language", "zh-cn")
	values.Set("details", "true")
	rawURL, err := c.join(c.accuWeatherBase, "/currentconditions/v1/"+url.PathEscape(locationKey), values)
	if err != nil {
		return Current{}, err
	}
	var raw []accuCurrent
	if err := c.getJSON(ctx, ProviderAccuWeather, rawURL, &raw); err != nil {
		return Current{}, err
	}
	if len(raw) == 0 || raw[0].Temperature.Metric.Value == nil {
		return Current{}, fmt.Errorf("%w: accuweather current weather is incomplete", contracts.ErrDataIncomplete)
	}
	item := raw[0]
	observedAt, err := parseRFC3339(item.LocalObservationDateTime)
	if err != nil {
		return Current{}, err
	}
	current := Current{
		ObservedAt:           observedAt,
		TemperatureC:         *item.Temperature.Metric.Value,
		WeatherText:          firstNonEmpty(item.WeatherText, "未知天气"),
		WeatherCode:          accuWeatherCode(item.WeatherIcon, item.WeatherText),
		HumidityPercent:      item.RelativeHumidity,
		WindDirectionDeg:     item.Wind.Direction.Degrees,
		ApparentTemperatureC: item.RealFeelTemperature.Metric.Value,
		PrecipitationMM:      item.PrecipitationSummary.Precipitation.Metric.Value,
	}
	if item.Wind.Speed.Metric.Value != nil {
		speed := metricSpeedToMps(*item.Wind.Speed.Metric.Value, item.Wind.Speed.Metric.Unit)
		current.WindSpeedMps = &speed
	}
	if item.Visibility.Metric.Value != nil {
		vis := *item.Visibility.Metric.Value
		if strings.EqualFold(item.Visibility.Metric.Unit, "km") {
			vis *= 1000
		}
		current.VisibilityM = &vis
	}
	return current, nil
}

func (c *Client) accuHourly(ctx context.Context, locationKey string, hours int) ([]HourlyPoint, error) {
	values := url.Values{}
	values.Set("apikey", c.accuWeatherAPIKey)
	values.Set("language", "zh-cn")
	values.Set("metric", "true")
	values.Set("details", "true")
	rawURL, err := c.join(c.accuWeatherBase, "/forecasts/v1/hourly/12hour/"+url.PathEscape(locationKey), values)
	if err != nil {
		return nil, err
	}
	var raw []accuHourly
	if err := c.getJSON(ctx, ProviderAccuWeather, rawURL, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: accuweather hourly weather is incomplete", contracts.ErrDataIncomplete)
	}
	limit := hours
	if limit > len(raw) {
		limit = len(raw)
	}
	points := make([]HourlyPoint, 0, limit)
	for i := 0; i < limit; i++ {
		item := raw[i]
		if item.Temperature.Value == nil {
			return nil, fmt.Errorf("%w: accuweather hourly weather is incomplete", contracts.ErrDataIncomplete)
		}
		when, err := parseRFC3339(item.DateTime)
		if err != nil {
			return nil, err
		}
		point := HourlyPoint{
			Time:                     when,
			TemperatureC:             *item.Temperature.Value,
			WeatherText:              firstNonEmpty(item.IconPhrase, "未知天气"),
			WeatherCode:              accuWeatherCode(item.WeatherIcon, item.IconPhrase),
			HumidityPercent:          item.RelativeHumidity,
			PrecipitationProbability: item.PrecipitationProbability,
			PrecipitationMM:          item.Rain.Value,
		}
		if item.Wind.Speed.Value != nil {
			speed := metricSpeedToMps(*item.Wind.Speed.Value, item.Wind.Speed.Unit)
			point.WindSpeedMps = &speed
		}
		points = append(points, point)
	}
	return points, nil
}

func normalizeAccuAlerts(raw []accuAlert) []Alert {
	alerts := make([]Alert, 0, len(raw))
	for i, item := range raw {
		headline := strings.TrimSpace(item.Description.Localized)
		if headline == "" {
			continue
		}
		id := strings.TrimSpace(string(item.AlertID))
		id = strings.Trim(id, `"`)
		if id == "" {
			id = fmt.Sprintf("accuweather-%d", i+1)
		}
		alert := Alert{
			ID:       id,
			Headline: headline,
			Severity: alertSeverity(firstNonEmpty(item.Type, string(item.Severity))),
			Event:    item.Category,
			Source:   firstNonEmpty(item.Source, "accuweather"),
		}
		if len(item.Area) > 0 {
			if onset, err := parseRFC3339(item.Area[0].StartTime); err == nil {
				alert.Onset = &onset
			}
			if ends, err := parseRFC3339(item.Area[0].EndTime); err == nil {
				alert.Ends = &ends
			}
		}
		alerts = append(alerts, alert)
	}
	return alerts
}

func metricSpeedToMps(value float64, unit string) float64 {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "km/h", "kmh":
		return value / 3.6
	case "mi/h", "mph":
		return value * 0.44704
	default:
		return value
	}
}

func accuWeatherCode(icon int, text string) int {
	switch icon {
	case 1, 2, 3:
		return 0
	case 4, 5, 6:
		return 2
	case 7, 8:
		return 3
	case 11:
		return 45
	case 12, 13, 14:
		return 63
	case 15, 16, 17:
		return 95
	case 18:
		return 65
	case 19, 20, 21, 22, 23:
		return 71
	case 24, 25, 26:
		return 66
	case 29:
		return 67
	case 32:
		return 45
	}
	return xiaomiWeatherCode(text)
}
