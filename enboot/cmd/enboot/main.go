// enboot runs a program under enboot, for a program that does
// not embed its own:
//
//	enboot <program> [args...]
//
// A Go program has no need of it — enboot.Wrap() makes the program its own
// enboot. This is for everything else: the tenant's side of PROTOCOL.md is
// a socket and eight verbs, in any language.
package main

import (
	"fmt"
	"os"

	"github.com/neuroplastio/engram/enboot"
)

// Set at build time: -ldflags "-X main.version=… -X main.commit=…".
var version, commit = "dev", "unknown"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("enboot %s (%s) protocol %d\n", version, commit, enboot.Protocol)
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: enboot <program> [args...]")
		os.Exit(2)
	}
	enboot.Run(os.Args[1], os.Args[2:])
}
