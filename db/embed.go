// Package dbfs embarque les migrations SQL dans le binaire.
package dbfs

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
