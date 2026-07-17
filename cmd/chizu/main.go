// Command chizu is the single binary for every chizu plane: crawl, build,
// serve, root, and the dev harness. Spec 2107 defines the surface; planes
// land milestone by milestone.
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("chizu " + version)
	default:
		fmt.Fprintf(os.Stderr, "chizu: unknown command %q (nothing is implemented yet; milestone C0a lands first)\n", os.Args[1])
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: chizu <command>")
	fmt.Fprintln(os.Stderr, "commands arrive milestone by milestone; only version exists today")
}
