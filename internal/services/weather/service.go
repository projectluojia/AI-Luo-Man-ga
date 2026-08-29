package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	wx "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/weather"
)

// Invoker 是 Service 调用原子 Tool 的治理入口，生产路径为 Dispatcher.UseTool。
type Invoker interface {
	UseTool(ctx context.Context, request contracts.RequestContext, serviceID, toolID string, payload json.RawMessage) (json.RawMessage, error)
}

type Service struct {
	invoker Invoker
	client  *wx.Client
	now     func() time.Time
}

func NewService(invoker Invoker, client *wx.Client) *Service {
	return NewServiceWithClock(invoker, client, time.Now)
}

func NewServiceWithClock(invoker Invoker, client *wx.Client, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{invoker: invoker, client: client, now: now}
}

func (s *Service) current(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	forecast, err := s.forecast(ctx, request, payload)
	if err != nil {
		return nil, err
	}
	return marshalView(wx.CurrentView{
		DataStatus: forecast.DataStatus,
		Location:   forecast.Location,
		Current:    forecast.Current,
	})
}

func (s *Service) hourly(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	forecast, err := s.forecast(ctx, request, payload)
	if err != nil {
		return nil, err
	}
	return marshalView(wx.HourlyView{
		DataStatus: forecast.DataStatus,
		Location:   forecast.Location,
		Hourly:     forecast.Hourly,
	})
}

func (s *Service) aqi(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	lookup, location, err := s.resolve(ctx, request, payload)
	if err != nil {
		return nil, err
	}
	query := lookup.LocationQuery()
	query.Latitude = location.Latitude
	query.Longitude = location.Longitude
	var last error
	for _, provider := range providersFor(lookup.Source) {
		result, err := s.airQualityFrom(ctx, request, provider, query, location)
		if err == nil {
			observe.Info(ctx, "天气空气质量查询完成",
				observe.StringAttr("provider", result.DataStatus.Provider),
				observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
			)
			return marshalView(result)
		}
		last = err
		if lookup.Source != wx.ProviderAuto && !errors.Is(err, wx.ErrProviderDisabled) {
			return nil, err
		}
	}
	if last == nil {
		last = fmt.Errorf("%w: weather air quality", contracts.ErrDataUnavailable)
	}
	return nil, governedUnavailable(last)
}

func (s *Service) alerts(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	lookup, location, err := s.resolve(ctx, request, payload)
	if err != nil {
		return nil, err
	}
	query := lookup.LocationQuery()
	query.Latitude = location.Latitude
	query.Longitude = location.Longitude
	var (
		last    error
		merged  []wx.Alert
		status  wx.DataStatus
		found   bool
		sources []string
	)
	for _, provider := range alertProvidersFor(lookup.Source) {
		result, err := s.alertsFrom(ctx, request, provider, query, location)
		if err != nil {
			last = err
			if lookup.Source != wx.ProviderAuto && !errors.Is(err, wx.ErrProviderDisabled) {
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
			last = fmt.Errorf("%w: weather alerts", contracts.ErrDataUnavailable)
		}
		return nil, governedUnavailable(last)
	}
	if merged == nil {
		merged = []wx.Alert{}
	}
	if len(sources) > 1 {
		status.Source = "weather-alerts"
		status.Provider = wx.ProviderAuto
	}
	governed, err := status.Govern(s.now())
	if err != nil {
		return nil, err
	}
	observe.Info(ctx, "天气预警查询完成",
		observe.IntAttr("alert_count", len(merged)),
		observe.IntAttr("provider_count", len(sources)),
	)
	return marshalView(wx.AlertsResult{
		DataStatus: governed,
		Location:   location,
		Alerts:     merged,
	})
}

func (s *Service) forecast(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (wx.ForecastResult, error) {
	lookup, location, err := s.resolve(ctx, request, payload)
	if err != nil {
		return wx.ForecastResult{}, err
	}
	query := lookup.LocationQuery()
	query.Latitude = location.Latitude
	query.Longitude = location.Longitude
	var last error
	for _, provider := range providersFor(lookup.Source) {
		result, err := s.forecastFrom(ctx, request, provider, query, location)
		if err == nil {
			observe.Info(ctx, "天气预报查询完成",
				observe.StringAttr("provider", result.DataStatus.Provider),
				observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
				observe.IntAttr("hourly_count", len(result.Hourly)),
			)
			return result, nil
		}
		last = err
		if lookup.Source != wx.ProviderAuto && !errors.Is(err, wx.ErrProviderDisabled) {
			return wx.ForecastResult{}, err
		}
	}
	if last == nil {
		last = fmt.Errorf("%w: weather forecast", contracts.ErrDataUnavailable)
	}
	return wx.ForecastResult{}, governedUnavailable(last)
}

func (s *Service) resolve(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (wx.LookupRequest, wx.Location, error) {
	lookup, err := decodeLookup(payload)
	if err != nil {
		return wx.LookupRequest{}, wx.Location{}, err
	}
	if lookup.Latitude != nil && lookup.Longitude != nil {
		location := wx.Location{
			Place:     lookup.Place,
			Latitude:  *lookup.Latitude,
			Longitude: *lookup.Longitude,
			Timezone:  wx.DefaultTimezone,
		}
		if location.Place == "" {
			location.Place = fmt.Sprintf("%.3f,%.3f", location.Latitude, location.Longitude)
		}
		if lookup.Place == wx.DefaultPlace && location.Latitude == wx.DefaultLatitude && location.Longitude == wx.DefaultLongitude {
			location.Country = "中国"
			location.Admin1 = "湖北省"
		}
		return lookup, location, nil
	}
	if lookup.Place == "" {
		return lookup, wx.Location{
			Place: wx.DefaultPlace, Latitude: wx.DefaultLatitude, Longitude: wx.DefaultLongitude,
			Timezone: wx.DefaultTimezone, Country: "中国", Admin1: "湖北省",
		}, nil
	}
	body, err := json.Marshal(wx.GeocodeQuery{Place: lookup.Place})
	if err != nil {
		return wx.LookupRequest{}, wx.Location{}, err
	}
	raw, err := s.invoker.UseTool(ctx, request, ServiceID, wx.OpenMeteoGeocodeToolID, body)
	if err != nil {
		if errors.Is(err, wx.ErrPlaceNotFound) {
			return wx.LookupRequest{}, wx.Location{}, errors.Join(registry.ErrSchemaValidation, err)
		}
		return wx.LookupRequest{}, wx.Location{}, err
	}
	var result wx.GeocodeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return wx.LookupRequest{}, wx.Location{}, fmt.Errorf("%w: geocode result is invalid", contracts.ErrDataIncomplete)
	}
	if _, err := result.DataStatus.Govern(s.now()); err != nil {
		return wx.LookupRequest{}, wx.Location{}, err
	}
	if lookup.Latitude == nil {
		lookup.Latitude = &result.Location.Latitude
		lookup.Longitude = &result.Location.Longitude
	}
	return lookup, result.Location, nil
}

func (s *Service) forecastFrom(ctx context.Context, request contracts.RequestContext, provider string, query wx.LocationQuery, location wx.Location) (wx.ForecastResult, error) {
	switch provider {
	case wx.ProviderOpenMeteo:
		raw, err := s.useLocationTool(ctx, request, wx.OpenMeteoForecastToolID, query)
		if err != nil {
			return wx.ForecastResult{}, err
		}
		var result wx.ForecastResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.ForecastResult{}, fmt.Errorf("%w: openmeteo forecast result is invalid", contracts.ErrDataIncomplete)
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	case wx.ProviderXiaomi:
		raw, err := s.useLocationTool(ctx, request, wx.XiaomiFetchToolID, query)
		if err != nil {
			return wx.ForecastResult{}, err
		}
		var result wx.XiaomiResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.ForecastResult{}, fmt.Errorf("%w: xiaomi forecast result is invalid", contracts.ErrDataIncomplete)
		}
		out := wx.ForecastResult{DataStatus: result.DataStatus, Location: overlayPlace(result.Location, location), Current: result.Current, Hourly: result.Hourly}
		return out, nil
	case wx.ProviderAccuWeather:
		if s.client != nil && !s.client.AccuWeatherEnabled() {
			return wx.ForecastResult{}, fmt.Errorf("%w: accuweather", wx.ErrProviderDisabled)
		}
		raw, err := s.useLocationTool(ctx, request, wx.AccuWeatherFetchToolID, query)
		if err != nil {
			return wx.ForecastResult{}, err
		}
		var result wx.ForecastResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.ForecastResult{}, fmt.Errorf("%w: accuweather forecast result is invalid", contracts.ErrDataIncomplete)
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	default:
		return wx.ForecastResult{}, fmt.Errorf("%w: unknown weather provider", wx.ErrInvalidRequest)
	}
}

func (s *Service) airQualityFrom(ctx context.Context, request contracts.RequestContext, provider string, query wx.LocationQuery, location wx.Location) (wx.AirQualityResult, error) {
	switch provider {
	case wx.ProviderOpenMeteo:
		raw, err := s.useLocationTool(ctx, request, wx.OpenMeteoAirQualityToolID, query)
		if err != nil {
			return wx.AirQualityResult{}, err
		}
		var result wx.AirQualityResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.AirQualityResult{}, fmt.Errorf("%w: openmeteo aqi result is invalid", contracts.ErrDataIncomplete)
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	case wx.ProviderXiaomi:
		raw, err := s.useLocationTool(ctx, request, wx.XiaomiFetchToolID, query)
		if err != nil {
			return wx.AirQualityResult{}, err
		}
		var result wx.XiaomiResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.AirQualityResult{}, fmt.Errorf("%w: xiaomi aqi result is invalid", contracts.ErrDataIncomplete)
		}
		if result.AQI == nil {
			return wx.AirQualityResult{}, fmt.Errorf("%w: xiaomi aqi is incomplete", contracts.ErrDataIncomplete)
		}
		return wx.AirQualityResult{DataStatus: result.DataStatus, Location: overlayPlace(result.Location, location), AQI: *result.AQI}, nil
	default:
		return wx.AirQualityResult{}, fmt.Errorf("%w: %s", wx.ErrProviderDisabled, provider)
	}
}

func (s *Service) alertsFrom(ctx context.Context, request contracts.RequestContext, provider string, query wx.LocationQuery, location wx.Location) (wx.AlertsResult, error) {
	switch provider {
	case wx.ProviderXiaomi:
		raw, err := s.useLocationTool(ctx, request, wx.XiaomiFetchToolID, query)
		if err != nil {
			return wx.AlertsResult{}, err
		}
		var result wx.XiaomiResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.AlertsResult{}, fmt.Errorf("%w: xiaomi alerts result is invalid", contracts.ErrDataIncomplete)
		}
		alerts := result.Alerts
		if alerts == nil {
			alerts = []wx.Alert{}
		}
		return wx.AlertsResult{DataStatus: result.DataStatus, Location: overlayPlace(result.Location, location), Alerts: alerts}, nil
	case wx.ProviderAccuWeather:
		if s.client != nil && !s.client.AccuWeatherEnabled() {
			return wx.AlertsResult{}, fmt.Errorf("%w: accuweather", wx.ErrProviderDisabled)
		}
		raw, err := s.useLocationTool(ctx, request, wx.AccuWeatherAlertsToolID, query)
		if err != nil {
			return wx.AlertsResult{}, err
		}
		var result wx.AlertsResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return wx.AlertsResult{}, fmt.Errorf("%w: accuweather alerts result is invalid", contracts.ErrDataIncomplete)
		}
		if result.Alerts == nil {
			result.Alerts = []wx.Alert{}
		}
		result.Location = overlayPlace(result.Location, location)
		return result, nil
	default:
		return wx.AlertsResult{}, fmt.Errorf("%w: %s", wx.ErrProviderDisabled, provider)
	}
}

func (s *Service) useLocationTool(ctx context.Context, request contracts.RequestContext, toolID string, query wx.LocationQuery) (json.RawMessage, error) {
	body, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	return s.invoker.UseTool(ctx, request, ServiceID, toolID, body)
}

func decodeLookup(payload json.RawMessage) (wx.LookupRequest, error) {
	var request wx.LookupRequest
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := jsonutil.DecodeStrict(payload, &request); err != nil {
		return wx.LookupRequest{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	if err := request.NormalizeAndValidate(); err != nil {
		return wx.LookupRequest{}, errors.Join(registry.ErrSchemaValidation, err)
	}
	return request, nil
}

func providersFor(source string) []string {
	switch source {
	case wx.ProviderOpenMeteo:
		return []string{wx.ProviderOpenMeteo}
	case wx.ProviderXiaomi:
		return []string{wx.ProviderXiaomi}
	case wx.ProviderAccuWeather:
		return []string{wx.ProviderAccuWeather}
	default:
		return []string{wx.ProviderOpenMeteo, wx.ProviderXiaomi, wx.ProviderAccuWeather}
	}
}

func alertProvidersFor(source string) []string {
	switch source {
	case wx.ProviderOpenMeteo:
		return nil
	case wx.ProviderXiaomi:
		return []string{wx.ProviderXiaomi}
	case wx.ProviderAccuWeather:
		return []string{wx.ProviderAccuWeather}
	default:
		return []string{wx.ProviderXiaomi, wx.ProviderAccuWeather}
	}
}

func governedUnavailable(err error) error {
	if errors.Is(err, wx.ErrProviderDisabled) {
		return errors.Join(contracts.ErrDataUnavailable, err)
	}
	return err
}

func overlayPlace(base, resolved wx.Location) wx.Location {
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

func marshalView(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode weather capability result: %w", err)
	}
	return encoded, nil
}
