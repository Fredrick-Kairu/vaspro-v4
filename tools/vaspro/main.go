// Command vaspro is VASPRO's operational CLI. Today it has one
// subcommand (packs validate); more are expected to join it over time,
// hence the plain dispatch below rather than a one-subcommand framework.
package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	switch args[0] {
	case "packs":
		return runPacks(args[1:])
	default:
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vaspro packs validate [pack.yaml ...]")
}

func runPacks(args []string) int {
	if len(args) == 0 || args[0] != "validate" {
		usage()
		return 2
	}

	packPaths := args[1:]
	if len(packPaths) == 0 {
		found, err := discoverPacks(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, "vaspro:", err)
			return 1
		}
		packPaths = found
	}
	if len(packPaths) == 0 {
		fmt.Fprintln(os.Stderr, "vaspro: no packs found under packs/*/pack.yaml")
		return 1
	}

	if packsValidate(os.Stdout, defaultSchemaPath, packPaths) {
		return 0
	}
	return 1
}
