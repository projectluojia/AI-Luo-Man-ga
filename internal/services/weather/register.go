package weather

import (
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	wx "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/weather"
)

const (
	ServiceID           = wx.ServiceID
	CurrentCapabilityID = "weather.current"
	HourlyCapabilityID  = "weather.hourly"
	AQICapabilityID     = "weather.aqi"
	AlertsCapabilityID  = "weather.alerts"
)

func ToolIDs() []string {
	specs := wx.ToolSpecs()
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.ID)
	}
	return ids
}

func CapabilityIDs() []string {
	return []string{CurrentCapabilityID, HourlyCapabilityID, AQICapabilityID, AlertsCapabilityID}
}

func Register(reg *registry.Registry, service *Service) error {
	if reg == nil || service == nil || service.invoker == nil || service.client == nil {
		return registry.ErrInvalidSpec
	}
	return reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID:               ServiceID,
			Version:          "1.0.0",
			Description:      "Campus weather lookup over Open-Meteo, Xiaomi Weather and AccuWeather.",
			ToolDependencies: ToolIDs(),
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			CurrentCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID: CurrentCapabilityID, Version: "1.0.0", Name: "查询当前天气",
					Description:     "Get current weather for a place or coordinates. Defaults to Wuhan University Luojia Hill when no location is given.",
					ServiceID:       ServiceID,
					InputSchemaJSON: wx.LookupInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: service.current,
			},
			HourlyCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID: HourlyCapabilityID, Version: "1.0.0", Name: "查询逐小时天气",
					Description:     "Get hourly weather forecast for up to 48 hours. Defaults to Wuhan University Luojia Hill when no location is given.",
					ServiceID:       ServiceID,
					InputSchemaJSON: wx.LookupInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: service.hourly,
			},
			AQICapabilityID: {
				Spec: registry.CapabilitySpec{
					ID: AQICapabilityID, Version: "1.0.0", Name: "查询空气质量",
					Description:     "Get air quality index for a place or coordinates. Defaults to Wuhan University Luojia Hill when no location is given.",
					ServiceID:       ServiceID,
					InputSchemaJSON: wx.LookupInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: service.aqi,
			},
			AlertsCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID: AlertsCapabilityID, Version: "1.0.0", Name: "查询天气预警",
					Description:     "Get weather alerts for a place or coordinates. Empty alerts mean the provider currently reports none; missing providers return a governed unavailable error.",
					ServiceID:       ServiceID,
					InputSchemaJSON: wx.LookupInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: service.alerts,
			},
		},
	})
}
