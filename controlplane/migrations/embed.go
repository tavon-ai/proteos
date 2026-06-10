// Package migrations embeds the golang-migrate SQL files so the control-plane
// binary can apply them on startup without shipping the .sql files separately.
package migrations

import "embed"

// FS holds the embedded up/down migration files.
//
//go:embed *.sql
var FS embed.FS
