// Package migrations embeds the versioned SQL migrations into the binary.
//
// Embedding is what makes cmd/migrate a single deployable artifact: the API image
// carries its own schema history, so "which migrations does this build know
// about?" has an answer that cannot drift from the running code. There is no
// runtime dependency on the repository layout or on a mounted volume.
package migrations

import "embed"

// FS holds every .sql file in this directory.
//
// The pattern is anchored to *.sql (not **/*.sql) so the migration set stays flat
// and the version ordering is unambiguous.
//
//go:embed *.sql
var FS embed.FS
