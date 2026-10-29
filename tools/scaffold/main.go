// Command scaffold generates a new service. Run via
// `make new-service NAME=<svc>`.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	name := flag.String("name", "", "service name, e.g. identity")
	flag.Parse()

	if *name == "" {
		fmt.Fprintln(os.Stderr, "scaffold: -name is required")
		os.Exit(2)
	}

	if err := Generate(".", *name); err != nil {
		fmt.Fprintln(os.Stderr, "scaffold:", err)
		os.Exit(1)
	}
	fmt.Printf("scaffold: generated %s\n", *name)
}
