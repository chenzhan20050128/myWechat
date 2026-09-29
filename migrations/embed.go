// Package migrations embeds the goose SQL migrations (MySQL dialect).
// Keep this file and the .sql files together; cmd/migrate consumes FS.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
