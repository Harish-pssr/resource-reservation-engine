// Package migrations embeds the SQL files so a binary needs nothing but itself to set up a database.
package migrations

import "embed"

// FS holds core/*.sql (tables shared by both schema variants), ops/*.sql (fault-injection settings, also in
// both) and backstop/*.sql (the unique
// index that makes a double sale impossible, D2). The unprotected variant used as the
// negative control gets core only.
//
//go:embed core/*.sql backstop/*.sql ops/*.sql
var FS embed.FS
