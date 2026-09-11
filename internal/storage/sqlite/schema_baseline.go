package sqlite

import _ "embed"

const schemaBaselineVersion = 31

//go:embed schema.sql
var schemaBaselineSQL string

func init() {
	registerMigration(schemaBaselineVersion, schemaBaselineSQL)
}
