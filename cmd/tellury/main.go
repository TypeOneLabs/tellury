// Command tellury finds and prices cloud waste.
package main

import (
	"fmt"
	"os"

	"github.com/TypeOneLabs/tellury/internal/cli"
)

func main() {
	code, err := cli.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tellury: "+err.Error())
	}
	os.Exit(code)
}
