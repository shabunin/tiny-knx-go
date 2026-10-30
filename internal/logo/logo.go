// Package logo has the logo of tiny-knx-go, which the command line tools print when they start.
package logo

import (
	"fmt"
	"os"
)

// Mouse is the logo.
const Mouse = `  ()-()
   \"/_
    '  )`

// Print writes the logo to stderr, so that it stays out of the output of commands.
func Print() {
	fmt.Fprintf(os.Stderr, "\n%s\n\n", Mouse)
}
