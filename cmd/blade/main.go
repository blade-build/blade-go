// Command blade is a Go reimplementation of the Blade build system.
//
// Work in progress: the goal is to build the C++ RPC framework "flare" with
// sufficient tests and coverage. See README.md for the phased plan.
package main

import (
	"fmt"

	"github.com/blade-build/blade-go/internal/version"
)

func main() {
	fmt.Printf("blade-go %s (reimplementation in progress)\n", version.Version)
}
