// Package migrations embeds the ordered SQL schema migrations.
package migrations

import "embed"

// FS holds every NNNN_name.sql file; they are applied in lexical order.
//
//go:embed *.sql
var FS embed.FS
