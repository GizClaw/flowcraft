// Package migrations embeds SQL migration files for goose.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
