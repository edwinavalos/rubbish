// Package store holds the embedded goose migrations shared by all
// Postgres-backed repositories under the `rubbish` schema.
package store

import "embed"

//go:embed migrations/*.sql
var MigrationsFS embed.FS

// MigrationsDir is the embed.FS subdirectory goose expects to walk.
const MigrationsDir = "migrations"
