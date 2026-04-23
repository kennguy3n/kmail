// Command kmail-migration is the Migration Orchestrator
// entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): orchestrate
// Gmail / IMAP imports with tenant-scoped imapsync workers,
// checkpoint/resume, staged sync, and a cutover checklist.
//
// This binary also applies docs/SCHEMA.md migrations — see the
// migrate subcommand (not yet implemented).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-migration: not yet implemented")
	os.Exit(1)
}
