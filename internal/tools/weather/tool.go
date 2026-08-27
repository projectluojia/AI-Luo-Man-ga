package weather

import (
	"context"
	"encoding/json"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// ToolSpecs 返回天气原子 Tool 规格（单一来源，供 Registry 注册共用）。
func ToolSpecs() []registry.ToolSpec {
	return []registry.ToolSpec{
		{
			ID: OpenMeteoForecastToolID, Version: "1.0.0",
			Description:     "Fetch current and hourly forecast from Open-Meteo.",
			InputSchemaJSON: LocationInputSchemaJSON, SideEffect: registry.SideEffectRead,
		},
		{
			ID: OpenMeteoAirQualityToolID, Version: "1.0.0",
			Description:     "Fetch air quality index from Open-Meteo.",
			InputSchemaJSON: LocationInputSchemaJSON, SideEffect: registry.SideEffectRead,
		},
		{
			ID: OpenMeteoGeocodeToolID, Version: "1.0.0",
			Description:     "Resolve a place name to coordinates using Open-Meteo geocoding.",
			InputSchemaJSON: GeocodeInputSchemaJSON, SideEffect: registry.SideEffectRead,
		},
		{
			ID: XiaomiFetchToolID, Version: "1.0.0",
			Description:     "Fetch current, hourly, AQI and alerts from Xiaomi Weather.",
			InputSchemaJSON: LocationInputSchemaJSON, SideEffect: registry.SideEffectRead,
		},
		{
			ID: AccuWeatherFetchToolID, Version: "1.0.0",
			Description:     "Fetch current and hourly forecast from AccuWeather.",
			InputSchemaJSON: LocationInputSchemaJSON, SideEffect: registry.SideEffectRead,
		},
		{
			ID: AccuWeatherAlertsToolID, Version: "1.0.0",
			Description:     "Fetch weather alerts from AccuWeather.",
			InputSchemaJSON: LocationInputSchemaJSON, SideEffect: registry.SideEffectRead,
		},
	}
}

// ToolHandlers 返回天气 Tool 执行器。
func ToolHandlers(client *Client) map[string]registry.Handler {
	return map[string]registry.Handler{
		OpenMeteoForecastToolID:   forecastHandler(client),
		OpenMeteoAirQualityToolID: airQualityHandler(client),
		OpenMeteoGeocodeToolID:    geocodeHandler(client),
		XiaomiFetchToolID:         xiaomiHandler(client),
		AccuWeatherFetchToolID:    accuFetchHandler(client),
		AccuWeatherAlertsToolID:   accuAlertsHandler(client),
	}
}

func RegisterTools(reg *registry.Registry, client *Client) error {
	if reg == nil || client == nil {
		return registry.ErrInvalidSpec
	}
	handlers := ToolHandlers(client)
	for _, spec := range ToolSpecs() {
		if err := reg.RegisterTool(registry.ToolRegistration{Spec: spec, Handler: handlers[spec.ID]}); err != nil {
			return err
		}
	}
	return nil
}

func forecastHandler(client *Client) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		query, err := decodeLocation(payload)
		if err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始查询 Open-Meteo 预报",
			observe.StringAttr("tool_id", OpenMeteoForecastToolID),
			observe.IntAttr("hours", query.Hours),
		)
		result, err := client.OpenMeteoForecast(ctx, request.AppID, query)
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "Open-Meteo 预报查询完成",
			observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
			observe.IntAttr("hourly_count", len(result.Hourly)),
			observe.Duration(started),
		)
		return marshalResult(result)
	}
}

func airQualityHandler(client *Client) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		query, err := decodeLocation(payload)
		if err != nil {
			return nil, err
		}
		result, err := client.OpenMeteoAirQuality(ctx, request.AppID, query)
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "Open-Meteo 空气质量查询完成",
			observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
			observe.Duration(started),
		)
		return marshalResult(result)
	}
}

func geocodeHandler(client *Client) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		query, err := decodeGeocode(payload)
		if err != nil {
			return nil, err
		}
		result, err := client.OpenMeteoGeocode(ctx, request.AppID, query)
		if err != nil {
			return nil, err
		}
		return marshalResult(result)
	}
}

func xiaomiHandler(client *Client) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		query, err := decodeLocation(payload)
		if err != nil {
			return nil, err
		}
		result, err := client.XiaomiFetch(ctx, request.AppID, query)
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "小米天气查询完成",
			observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
			observe.IntAttr("hourly_count", len(result.Hourly)),
			observe.Duration(started),
		)
		return marshalResult(result)
	}
}

func accuFetchHandler(client *Client) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		query, err := decodeLocation(payload)
		if err != nil {
			return nil, err
		}
		result, err := client.AccuWeatherFetch(ctx, request.AppID, query)
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "AccuWeather 预报查询完成",
			observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
			observe.IntAttr("hourly_count", len(result.Hourly)),
			observe.Duration(started),
		)
		return marshalResult(result)
	}
}

func accuAlertsHandler(client *Client) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		query, err := decodeLocation(payload)
		if err != nil {
			return nil, err
		}
		result, err := client.AccuWeatherAlerts(ctx, request.AppID, query)
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "AccuWeather 预警查询完成",
			observe.BoolAttr("cache_hit", result.DataStatus.CacheHit),
			observe.IntAttr("alert_count", len(result.Alerts)),
			observe.Duration(started),
		)
		return marshalResult(result)
	}
}
