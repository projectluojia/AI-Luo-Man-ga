package wx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Service 编排天气能力：定位解析 → 供应商分发 → fail-closed 数据治理。
// 与旧内核内服务不同，isolated provider 没有 Dispatcher，供应商客户端直接内联调用。
type Service struct {
	client *Client
	now    func() time.Time
}

func NewService(client *Client) *Service {
	return NewServiceWithClock(client, time.Now)
}

func NewServiceWithClock(client *Client, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{client: client, now: now}
}

func (s *Service) Current(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	forecast, err := s.forecast(ctx, appID, payload)
	if err != nil {
		return nil, err
	}
	return marshalResult(CurrentView{DataStatus: forecast.DataStatus, Location: forecast.Location, Current: forecast.Current})
}

func (s *Service) Hourly(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	forecast, err := s.forecast(ctx, appID, payload)
	if err != nil {
		return nil, err
	}
	return marshalResult(HourlyView{DataStatus: forecast.DataStatus, Location: forecast.Location, Hourly: forecast.Hourly})
}

func (s *Service) AQI(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	lookup, location, err := s.resolve(ctx, appID, payload)
	if err != nil {
		return nil, err
	}
	query := lookup.LocationQuery()
	query.Latitude = location.Latitude
	query.Longitude = location.Longitude
	var last error
	for _, provider := range providersFor(lookup.Source) {
		result, err := s.airQualityFrom(ctx, appID, provider, query, location)
		if err == nil {
			return marshalResult(result)
		}
		last = err
		if lookup.Source != ProviderAuto && !errors.Is(err, ErrProviderDisabled) {
			return nil, err
		}
	}
	if last == nil {
		last = dataUnavailable("weather air quality")
	}
	return nil, governedUnavailable(last)
}

func (s *Service) Alerts(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	lookup, location, err := s.resolve(ctx, appID, payload)
	if err != nil {
		return nil, err
	}
	query := lookup.LocationQuery()
	query.Latitude = location.Latitude
	query.Longitude = location.Longitude
	var (
		last    error
		merged  []Alert
		status  DataStatus
		found   bool
		sources []string
	)
	for _, provider := range alertProvidersFor(lookup.Source) {
		result, err := s.alertsFrom(ctx, appID, provider, query, location)
		if err != nil {
			last = err
			if lookup.Source != ProviderAuto && !errors.Is(err, ErrProviderDisabled) {
				return nil, err
			}
			continue
		}
		found = true
		if status.FetchedAt.IsZero() || result.DataStatus.ValidUntil.Before(status.ValidUntil) {
			status = result.DataStatus
		}
		sources = append(sources, result.DataStatus.Provider)
		merged = append(merged, result.Alerts...)
	}
	if !found {
		if last == nil {
			last = dataUnavailable("weather alerts")
		}
		return nil, governedUnavailable(last)
	}
	if merged == nil {
		merged = []Alert{}
	}
	if len(sources) > 1 {
		status.Source = "weather-alerts"
		status.Provider = ProviderAuto
	}
	governed, err := status.Govern(s.now())
	if err != nil {
		return nil, err
	}
	return marshalResult(AlertsResult{DataStatus: governed, Location: location, Alerts: merged})
}

func (s *Service) forecast(ctx context.Context, appID string, payload json.RawMessage) (ForecastResult, error) {
	lookup, location, err := s.resolve(ctx, appID, payload)
	if err != nil {
		return ForecastResult{}, err
	}
	query := lookup.LocationQuery()
	query.Latitude = location.Latitude
	query.Longitude = location.Longitude
	var last error
	for _, provider := range providersFor(lookup.Source) {
		result, err := s.forecastFrom(ctx, appID, provider, query, location)
		if err == nil {
			return result, nil
		}
		last = err
		if lookup.Source != ProviderAuto && !errors.Is(err, ErrProviderDisabled) {
			return ForecastResult{}, err
		}
	}
	if last == nil {
		last = dataUnavailable("weather forecast")
	}
	return ForecastResult{}, governedUnavailable(last)
}

func (s *Service) resolve(ctx context.Context, appID string, payload json.RawMessage) (LookupRequest, Location, error) {
	lookup, err := decodeLookup(payload)
	if err != nil {
		return LookupRequest{}, Location{}, err
	}
	if lookup.Latitude != nil && lookup.Longitude != nil {
		location := Location{
			Place:     lookup.Place,
			Latitude:  *lookup.Latitude,
			Longitude: *lookup.Longitude,
			Timezone:  DefaultTimezone,
		}
		if location.Place == "" {
			location.Place = fmt.Sprintf("%.3f,%.3f", location.Latitude, location.Longitude)
		}
		if lookup.Place == DefaultPlace && location.Latitude == DefaultLatitude && location.Longitude == DefaultLongitude {
			location.Country = "中国"
			location.Admin1 = "湖北省"
		}
		return lookup, location, nil
	}
	if lookup.Place == "" {
		return lookup, Location{
			Place: DefaultPlace, Latitude: DefaultLatitude, Longitude: DefaultLongitude,
			Timezone: DefaultTimezone, Country: "中国", Admin1: "湖北省",
		}, nil
	}
	result, err := s.client.OpenMeteoGeocode(ctx, appID, GeocodeQuery{Place: lookup.Place})
	if err != nil {
		if errors.Is(err, ErrPlaceNotFound) {
			return LookupRequest{}, Location{}, invalidArguments("place %q was not found", lookup.Place)
		}
		return LookupRequest{}, Location{}, err
	}
	if _, err := result.DataStatus.Govern(s.now()); err != nil {
		return LookupRequest{}, Location{}, err
	}
	if lookup.Latitude == nil {
		lookup.Latitude = &result.Location.Latitude
		lookup.Longitude = &result.Location.Longitude
	}
	return lookup, result.Location, nil
}

func (s *Service) forecastFrom(ctx context.Context, appID, provider string, query LocationQuery, location Location) (ForecastResult, error) {
	switch provider {
	case ProviderOpenMeteo:
		result, err := s.client.OpenMeteoForecast(ctx, appID, query)
		if err != nil {
			return ForecastResult{}, err
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	case ProviderXiaomi:
		result, err := s.client.XiaomiFetch(ctx, appID, query)
		if err != nil {
			return ForecastResult{}, err
		}
		return ForecastResult{DataStatus: result.DataStatus, Location: overlayPlace(result.Location, location), Current: result.Current, Hourly: result.Hourly}, nil
	case ProviderAccuWeather:
		if !s.client.AccuWeatherEnabled() {
			return ForecastResult{}, fmt.Errorf("%w: accuweather", ErrProviderDisabled)
		}
		result, err := s.client.AccuWeatherFetch(ctx, appID, query)
		if err != nil {
			return ForecastResult{}, err
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	default:
		return ForecastResult{}, fmt.Errorf("%w: unknown weather provider %q", ErrInvalidRequest, provider)
	}
}

func (s *Service) airQualityFrom(ctx context.Context, appID, provider string, query LocationQuery, location Location) (AirQualityResult, error) {
	switch provider {
	case ProviderOpenMeteo:
		result, err := s.client.OpenMeteoAirQuality(ctx, appID, query)
		if err != nil {
			return AirQualityResult{}, err
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	case ProviderXiaomi:
		result, err := s.client.XiaomiFetch(ctx, appID, query)
		if err != nil {
			return AirQualityResult{}, err
		}
		if result.AQI == nil {
			return AirQualityResult{}, dataIncomplete("xiaomi aqi is incomplete")
		}
		return AirQualityResult{DataStatus: result.DataStatus, Location: overlayPlace(result.Location, location), AQI: *result.AQI}, nil
	default:
		return AirQualityResult{}, fmt.Errorf("%w: %s", ErrProviderDisabled, provider)
	}
}

func (s *Service) alertsFrom(ctx context.Context, appID, provider string, query LocationQuery, location Location) (AlertsResult, error) {
	switch provider {
	case ProviderXiaomi:
		result, err := s.client.XiaomiFetch(ctx, appID, query)
		if err != nil {
			return AlertsResult{}, err
		}
		alerts := result.Alerts
		if alerts == nil {
			alerts = []Alert{}
		}
		return AlertsResult{DataStatus: result.DataStatus, Location: overlayPlace(result.Location, location), Alerts: alerts}, nil
	case ProviderAccuWeather:
		if !s.client.AccuWeatherEnabled() {
			return AlertsResult{}, fmt.Errorf("%w: accuweather", ErrProviderDisabled)
		}
		result, err := s.client.AccuWeatherAlerts(ctx, appID, query)
		if err != nil {
			return AlertsResult{}, err
		}
		if result.Alerts == nil {
			result.Alerts = []Alert{}
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	default:
		return AlertsResult{}, fmt.Errorf("%w: %s", ErrProviderDisabled, provider)
	}
}

// decodeLookup 严格解码能力入参：拒绝未知字段与尾随数据，失败映射到 invalid_arguments。
func decodeLookup(payload json.RawMessage) (LookupRequest, error) {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	var request LookupRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return LookupRequest{}, invalidArguments("weather request is malformed")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return LookupRequest{}, invalidArguments("weather request must contain exactly one value")
		}
		return LookupRequest{}, invalidArguments("weather request is malformed")
	}
	if err := request.NormalizeAndValidate(); err != nil {
		return LookupRequest{}, err
	}
	return request, nil
}

func providersFor(source string) []string {
	switch source {
	case ProviderOpenMeteo:
		return []string{ProviderOpenMeteo}
	case ProviderXiaomi:
		return []string{ProviderXiaomi}
	case ProviderAccuWeather:
		return []string{ProviderAccuWeather}
	default:
		return []string{ProviderOpenMeteo, ProviderXiaomi, ProviderAccuWeather}
	}
}

func alertProvidersFor(source string) []string {
	switch source {
	case ProviderOpenMeteo:
		return nil
	case ProviderXiaomi:
		return []string{ProviderXiaomi}
	case ProviderAccuWeather:
		return []string{ProviderAccuWeather}
	default:
		return []string{ProviderXiaomi, ProviderAccuWeather}
	}
}

func governedUnavailable(err error) error {
	// 供应商停用降级为 data_unavailable，但保留 ErrProviderDisabled 供调用方识别；
	// 其余领域错误原样返回。
	if errors.Is(err, ErrProviderDisabled) {
		return errors.Join(dataUnavailable("weather provider is not configured"), err)
	}
	var we *Error
	if errors.As(err, &we) {
		return we
	}
	return dataUnavailable("weather provider request failed")
}

func overlayPlace(base, resolved Location) Location {
	if resolved.Place != "" {
		base.Place = resolved.Place
	}
	if resolved.Country != "" {
		base.Country = resolved.Country
	}
	if resolved.Admin1 != "" {
		base.Admin1 = resolved.Admin1
	}
	if resolved.Timezone != "" && base.Timezone == "" {
		base.Timezone = resolved.Timezone
	}
	return base
}
