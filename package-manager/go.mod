module github.com/projectluojia/AI-Luo-Man-ga/package-manager

go 1.26

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/iancoleman/strcase v0.3.0
	github.com/projectluojia/AI-Luo-Man-ga/contracts v0.0.0
)

require github.com/Masterminds/semver/v3 v3.5.0 // indirect

replace github.com/projectluojia/AI-Luo-Man-ga/contracts => ../contracts
