package omptelegram

import _ "embed"

// DefaultConfig is the bundled configuration used when no default file exists.
//
//go:embed config.toml
var DefaultConfig string

// MigrationExtension is the bundled bridge-owned native migration command.
//
//go:embed extensions/omp-telegram-migrate.ts
var MigrationExtension []byte
