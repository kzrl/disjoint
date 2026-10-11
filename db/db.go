package db

import "embed"

// MigrationsFS contains the goose SQL migrations.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS
