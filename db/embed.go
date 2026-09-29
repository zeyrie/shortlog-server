// Package db embeds the SQL migrations so the server binary can apply them.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
