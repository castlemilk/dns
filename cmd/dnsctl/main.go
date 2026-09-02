package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr, newConnectClient); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dnsctl: %v\n", err)
		os.Exit(1)
	}
}
