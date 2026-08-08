package observe

import (
	"context"
	"log/slog"
)

type contextFieldsKey struct{}

func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	return context.WithValue(ctx, contextFieldsKey{}, mergeAttrs(Fields(ctx), attrs))
}

func mergeAttrs(base, overrides []slog.Attr) []slog.Attr {
	fields := append([]slog.Attr(nil), base...)
	positions := make(map[string]int, len(fields)+len(overrides))
	for index, field := range fields {
		positions[field.Key] = index
	}
	for _, attr := range overrides {
		if index, exists := positions[attr.Key]; exists {
			fields[index] = attr
			continue
		}
		positions[attr.Key] = len(fields)
		fields = append(fields, attr)
	}
	return fields
}

func Copy(source, target context.Context) context.Context {
	return With(target, Fields(source)...)
}

func Fields(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	fields, _ := ctx.Value(contextFieldsKey{}).([]slog.Attr)
	return append([]slog.Attr(nil), fields...)
}

func String(ctx context.Context, key string) string {
	fields := Fields(ctx)
	for index := len(fields) - 1; index >= 0; index-- {
		if fields[index].Key == key && fields[index].Value.Kind() == slog.KindString {
			return fields[index].Value.String()
		}
	}
	return ""
}
