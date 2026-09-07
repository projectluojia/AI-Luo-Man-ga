package sqlite

import _ "embed"

const schemaBaselineVersion = 30

//go:embed schema.sql
var schemaBaselineSQL string

func init() {
	registerMigration(schemaBaselineVersion, schemaBaselineSQL)
}
