package registry

import (
	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/strictschema"
)

const (
	maxSchemaBytes  = 64 << 10
	maxPayloadBytes = 64 << 10
)

func compileInputSchema(id, raw string) (*jsonschema.Schema, error) {
	return strictschema.CompileSchema(id, raw, "https://schema.invalid/ailuo/", maxSchemaBytes)
}

func validatePayload(schema *jsonschema.Schema, payload json.RawMessage) error {
	if err := strictschema.ValidatePayload(schema, payload, maxPayloadBytes); err != nil {
		return ErrSchemaValidation
	}
	return nil
}
