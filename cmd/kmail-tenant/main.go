// Command kmail-tenant is the Tenant Service entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): own the tenant
// lifecycle (create, suspend, delete, rename, rotate), user
// lifecycle, aliases, shared inboxes, and quota plan metadata.
// Authoritative for the control-plane Postgres schema defined in
// docs/SCHEMA.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-tenant: not yet implemented")
	os.Exit(1)
}
