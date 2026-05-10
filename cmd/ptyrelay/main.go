// Command ptyrelay is the ptyrelay CLI / library entry point.
//
// v0.1.0 ships only the library and a placeholder CLI; subcommands land in
// later milestones.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ptyrelay v0.1.0 — Shell-only MVP. CLI subcommands land in v0.3.0.")
	os.Exit(0)
}
