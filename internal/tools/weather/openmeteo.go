package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

type openMeteoForecastResponse struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
	Current   struct {
		Time                string   `json:"time"`
		Temperature2m       *float64 `json:"temperature_2m"`
		ApparentTemperature *float64 `json:"apparent_temperature"`
		RelativeHumidity2m  *int     `json:"relative_humidity_2m"`
		Precipitation       *float64 `json:"precipitation"`
		WeatherCode         *int     `json:"weather_code"`
		WindSpeed10m        *float64 `json:"wind_speed_10m"`
		WindDirection10m    *int     `json:"wind_direction_10m"`
		Visibility          *float64 `json:"visibility"`
	} `json:"current"`
	Hourly struct {
		Time                     []string   `json:"time"`
		Temperature2m            []*float64 `json:"temperature_2m"`
		RelativeHumidity2m       []*int     `json:"relative_humidity_2m"`
		Precipitation            []*float64 `json:"precipitation"`
		PrecipitationProbability []*int     `json:"precipitation_probability"`
		WeatherCode              []*int     `json:"weather_code"`
		WindSpeed10m             []*float64 `json:"wind_speed_10m"`
	} `json:"hourly"`
}

type openMeteoAirResponse struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
	Current   struct {
		Time        string   `json:"time"`
		USAQI       *int     `json:"us_aqi"`
		EuropeanAQI *int     `json:"european_aqi"`
		PM25        *float64 `json:"pm2_5"`
		PM10        *float64 `json:"pm10"`
	} `json:"current"`
}

type openMeteoGeocodeResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Timezone  string  `json:"timezone"`
		Country   string  `json:"country"`
		Admin1    string  `json:"admin1"`
	} `json:"results"`
}

func (c *Client) OpenMeteoForecast(ctx context.Context, appID string, query LocationQuery) (ForecastResult, error) {
	if err := query.NormalizeAndValidate(); err != nil {
		return ForecastResult{}, err
	}
	cacheKey := CacheKey(ProviderOpenMeteo, "forecast", query.Latitude, query.Longitude, query.Hours)
	if payload, entry, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return ForecastResult{}, err
	} else if ok {
		var result ForecastResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return ForecastResult{}, fmt.Errorf("%w: cached openmeteo forecast is invalid", contracts.ErrDataIncomplete)
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

	values := url.Values{}
	values.Set("latitude", formatCoord(query.Latitude))
	values.Set("longitude", formatCoord(query.Longitude))
	values.Set("timezone", "UTC")
	values.Set("wind_speed_unit", "ms")
	values.Set("forecast_hours", strconv.Itoa(query.Hours))
	values.Set("current", "temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m,wind_direction_10m,visibility")
	values.Set("hourly", "temperature_2m,relative_humidity_2m,precipitation,precipitation_probability,weather_code,wind_speed_10m")
	rawURL, err := c.join(c.openMeteoBase, "/v1/forecast", values)
	if err != nil {
		return ForecastResult{}, err
	}
	var raw openMeteoForecastResponse
	if err := c.getJSON(ctx, ProviderOpenMeteo, rawURL, &raw); err != nil {
		return ForecastResult{}, err
	}
	result, err := normalizeOpenMeteoForecast(query, raw, c.now())
	if err != nil {
		return ForecastResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ForecastResult{}, fmt.Errorf("encode openmeteo forecast: %w", err)
	}
	entry, err := c.store(ctx, appID, cacheKey, ProviderOpenMeteo, result.DataStatus.Revision, encoded, HourlyTTL)
	if err != nil {
		return ForecastResult{}, err
	}
	result.DataStatus.CacheHit = false
	result.DataStatus.Revision = entry.SourceRevision
	observe.Info(ctx, "Open-Meteo 预报已规范化",
		observe.StringAttr("provider", ProviderOpenMeteo),
		observe.IntAttr("hourly_count", len(result.Hourly)),
		observe.BoolAttr("cache_hit", false),
	)
	return result, nil
}

func (c *Client) OpenMeteoAirQuality(ctx context.Context, appID string, query LocationQuery) (AirQualityResult, error) {
	if err := query.NormalizeAndValidate(); err != nil {
		return AirQualityResult{}, err
	}
	cacheKey := CacheKey(ProviderOpenMeteo, "airquality", query.Latitude, query.Longitude)
	if payload, entry, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return AirQualityResult{}, err
	} else if ok {
		var result AirQualityResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return AirQualityResult{}, fmt.Errorf("%w: cached openmeteo aqi is invalid", contracts.ErrDataIncomplete)
		}
		result.DataStatus.CacheHit = true
		result.DataStatus.Revision = entry.SourceRevision
		governed, err := result.DataStatus.Govern(c.now())
		if err != nil {
			return AirQualityResult{}, err
		}
		result.DataStatus = governed
		return result, nil
	}
	values := url.Values{}
	values.Set("latitude", formatCoord(query.Latitude))
	values.Set("longitude", formatCoord(query.Longitude))
	values.Set("timezone", "UTC")
	values.Set("current", "us_aqi,european_aqi,pm2_5,pm10")
	rawURL, err := c.join(c.openMeteoAirBase, "/v1/air-quality", values)
	if err != nil {
		return AirQualityResult{}, err
	}
	var raw openMeteoAirResponse
	if err := c.getJSON(ctx, ProviderOpenMeteo, rawURL, &raw); err != nil {
		return AirQualityResult{}, err
	}
	result, err := normalizeOpenMeteoAir(query, raw, c.now())
	if err != nil {
		return AirQualityResult{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return AirQualityResult{}, fmt.Errorf("encode openmeteo aqi: %w", err)
	}
	entry, err := c.store(ctx, appID, cacheKey, ProviderOpenMeteo, result.DataStatus.Revision, encoded, AQITTL)
	if err != nil {
		return AirQualityResult{}, err
	}
	result.DataStatus.Revision = entry.SourceRevision
	observe.Info(ctx, "Open-Meteo 空气质量已规范化",
		observe.StringAttr("provider", ProviderOpenMeteo),
		observe.IntAttr("aqi_index", result.AQI.Index),
		observe.BoolAttr("cache_hit", false),
	)
	return result, nil
}

func (c *Client) OpenMeteoGeocode(ctx context.Context, appID string, query GeocodeQuery) (GeocodeResult, error) {
	if err := query.NormalizeAndValidate(); err != nil {
		return GeocodeResult{}, err
	}
	cacheKey := CacheKey(ProviderOpenMeteo, "geocode", query.Place)
	if payload, entry, ok, err := c.cached(ctx, appID, cacheKey); err != nil {
		return GeocodeResult{}, err
	} else if ok {
		var result GeocodeResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return GeocodeResult{}, fmt.Errorf("%w: cached geocode is invalid", contracts.ErrDataIncomplete)
		}
		result.DataStatus.CacheHit = true
		result.DataStatus.Revision = entry.SourceRevision
		governed, err := result.DataStatus.Govern(c.now())
		if err != nil {
			return GeocodeResult{}, err
		}
		result.DataStatus = governed
		return result, nil
	}
	values := url.Values{}
	values.Set("name", query.Place)
	values.Set("count", "1")
	values.Set("language", "zh")
	values.Set("format", "json")
	rawURL, err := c.join(c.openMeteoGeoBase, "/v1/search", values)
	if err != nil {
		return GeocodeResult{}, err
	}
	var raw openMeteoGeocodeResponse
	if err := c.getJSON(ctx, ProviderOpenMeteo, rawURL, &raw); err != nil {
		return GeocodeResult{}, err
	}
	if len(raw.Results) == 0 {
		return GeocodeResult{}, fmt.Errorf("%w: %v", ErrPlaceNotFound, query.Place)
	}
	hit := raw.Results[0]
	if err := validateCoordinates(hit.Latitude, hit.Longitude); err != nil {
		return GeocodeResult{}, fmt.Errorf("%w: geocode coordinates are invalid", contracts.ErrDataIncomplete)
	}
	fetchedAt := c.now().UTC()
	result := GeocodeResult{
		DataStatus: NewDataStatus(ProviderOpenMeteo, "open-meteo-geocoding", revision(ProviderOpenMeteo, fetchedAt), fetchedAt, fetchedAt.Add(GeocodeTTL), false),
		Location: Location{
			Place:     firstNonEmpty(hit.Name, query.Place),
			Latitude:  roundCoord(hit.Latitude),
			Longitude: roundCoord(hit.Longitude),
			Timezone:  firstNonEmpty(hit.Timezone, DefaultTimezone),
			Country:   hit.Country,
			Admin1:    hit.Admin1,
		},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return GeocodeResult{}, fmt.Errorf("encode geocode: %w", err)
	}
	entry, err := c.store(ctx, appID, cacheKey, ProviderOpenMeteo, result.DataStatus.Revision, encoded, GeocodeTTL)
	if err != nil {
		return GeocodeResult{}, err
	}
	result.DataStatus.Revision = entry.SourceRevision
	return result, nil
}

func normalizeOpenMeteoForecast(query LocationQuery, raw openMeteoForecastResponse, now time.Time) (ForecastResult, error) {
	if raw.Current.Temperature2m == nil || raw.Current.WeatherCode == nil || raw.Current.Time == "" {
		return ForecastResult{}, fmt.Errorf("%w: openmeteo current weather is incomplete", contracts.ErrDataIncomplete)
	}
	observedAt, err := parseRFC3339(raw.Current.Time)
	if err != nil {
		return ForecastResult{}, err
	}
	hourly, err := normalizeOpenMeteoHourly(raw, now, query.Hours)
	if err != nil {
		return ForecastResult{}, err
	}
	fetchedAt := now.UTC()
	return ForecastResult{
		DataStatus: NewDataStatus(ProviderOpenMeteo, "open-meteo-forecast", revision(ProviderOpenMeteo, fetchedAt), fetchedAt, fetchedAt.Add(HourlyTTL), false),
		Location: locationFromQuery(query, Location{
			Timezone: firstNonEmpty(raw.Timezone, DefaultTimezone),
			Latitude: raw.Latitude, Longitude: raw.Longitude,
		}),
		Current: Current{
			ObservedAt:           observedAt,
			Timezone:             firstNonEmpty(raw.Timezone, DefaultTimezone),
			TemperatureC:         *raw.Current.Temperature2m,
			ApparentTemperatureC: raw.Current.ApparentTemperature,
			HumidityPercent:      raw.Current.RelativeHumidity2m,
			WeatherCode:          *raw.Current.WeatherCode,
			WeatherText:          WeatherText(*raw.Current.WeatherCode),
			WindSpeedMps:         raw.Current.WindSpeed10m,
			WindDirectionDeg:     raw.Current.WindDirection10m,
			PrecipitationMM:      raw.Current.Precipitation,
			VisibilityM:          raw.Current.Visibility,
		},
		Hourly: hourly,
	}, nil
}

func normalizeOpenMeteoHourly(raw openMeteoForecastResponse, now time.Time, hours int) ([]HourlyPoint, error) {
	times := raw.Hourly.Time
	if len(times) == 0 {
		return nil, fmt.Errorf("%w: openmeteo hourly weather is incomplete", contracts.ErrDataIncomplete)
	}
	limit := hours
	if limit > len(times) {
		limit = len(times)
	}
	points := make([]HourlyPoint, 0, limit)
	cutoff := now.UTC().Add(-time.Hour)
	for i := 0; i < limit; i++ {
		when, err := parseRFC3339(times[i])
		if err != nil {
			return nil, err
		}
		if when.Before(cutoff) {
			continue
		}
		if i >= len(raw.Hourly.Temperature2m) || raw.Hourly.Temperature2m[i] == nil ||
			i >= len(raw.Hourly.WeatherCode) || raw.Hourly.WeatherCode[i] == nil {
			return nil, fmt.Errorf("%w: openmeteo hourly weather is incomplete", contracts.ErrDataIncomplete)
		}
		point := HourlyPoint{
			Time:         when,
			TemperatureC: *raw.Hourly.Temperature2m[i],
			WeatherCode:  *raw.Hourly.WeatherCode[i],
			WeatherText:  WeatherText(*raw.Hourly.WeatherCode[i]),
		}
		if i < len(raw.Hourly.RelativeHumidity2m) {
			point.HumidityPercent = raw.Hourly.RelativeHumidity2m[i]
		}
		if i < len(raw.Hourly.Precipitation) {
			point.PrecipitationMM = raw.Hourly.Precipitation[i]
		}
		if i < len(raw.Hourly.PrecipitationProbability) {
			point.PrecipitationProbability = raw.Hourly.PrecipitationProbability[i]
		}
		if i < len(raw.Hourly.WindSpeed10m) {
			point.WindSpeedMps = raw.Hourly.WindSpeed10m[i]
		}
		points = append(points, point)
		if len(points) >= hours {
			break
		}
	}
	if len(points) == 0 {
		return nil, fmt.Errorf("%w: openmeteo hourly weather is incomplete", contracts.ErrDataIncomplete)
	}
	return points, nil
}

func normalizeOpenMeteoAir(query LocationQuery, raw openMeteoAirResponse, now time.Time) (AirQualityResult, error) {
	if raw.Current.USAQI == nil && raw.Current.EuropeanAQI == nil {
		return AirQualityResult{}, fmt.Errorf("%w: openmeteo air quality is incomplete", contracts.ErrDataIncomplete)
	}
	observedAt := now.UTC()
	if raw.Current.Time != "" {
		if parsed, err := parseRFC3339(raw.Current.Time); err == nil {
			observedAt = parsed
		}
	}
	index := 0
	scale := "us-epa"
	if raw.Current.USAQI != nil {
		index = *raw.Current.USAQI
	} else {
		index = *raw.Current.EuropeanAQI
		scale = "european"
	}
	fetchedAt := now.UTC()
	return AirQualityResult{
		DataStatus: NewDataStatus(ProviderOpenMeteo, "open-meteo-air-quality", revision(ProviderOpenMeteo, fetchedAt), fetchedAt, fetchedAt.Add(AQITTL), false),
		Location: locationFromQuery(query, Location{
			Timezone: firstNonEmpty(raw.Timezone, DefaultTimezone),
			Latitude: raw.Latitude, Longitude: raw.Longitude,
		}),
		AQI: AQI{
			ObservedAt: observedAt,
			Index:      index,
			Scale:      scale,
			Category:   aqiCategory(index, scale),
			PM25:       raw.Current.PM25,
			PM10:       raw.Current.PM10,
		},
	}, nil
}

func formatCoord(value float64) string {
	return strconv.FormatFloat(value, 'f', 3, 64)
}
