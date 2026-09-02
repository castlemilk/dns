package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dns-restore: %v\n", err)
		os.Exit(1)
	}
}
