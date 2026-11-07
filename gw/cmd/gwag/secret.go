package main

import (
	"fmt"
	"io"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

// secretCmd implements `gwag secret gen`: print one random secret
// (48 random bytes, base64) that passes the declarations hook secret
// check.
func secretCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "gen" {
		fmt.Fprintln(stderr, "usage: gwag secret gen")
		return 2
	}
	s, err := gateway.GenerateSecret()
	if err != nil {
		fmt.Fprintf(stderr, "secret gen: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, s)
	return 0
}
