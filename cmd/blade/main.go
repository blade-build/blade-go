// Command blade is a Go reimplementation of the Blade build system.
//
// Work in progress: the goal is to build the C++ RPC framework "flare" with
// sufficient tests and coverage. See README.md for the phased plan.
package main

import (
	"fmt"
	"os"

	"github.com/blade-build/blade-go/internal/build"
	"github.com/blade-build/blade-go/internal/version"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "build" {
		if err := runBuild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "blade: "+err.Error())
			os.Exit(1)
		}
		return
	}
	fmt.Printf("blade-go %s (reimplementation in progress)\n", version.Version)
}

func runBuild(targets []string) error {
	if len(targets) == 0 {
		return fmt.Errorf("usage: blade build <target>...")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := build.FindRoot(cwd)
	if err != nil {
		return err
	}
	ninjaFile, err := build.Build(root, targets, build.Options{RunNinja: true})
	if err != nil {
		return err
	}
	fmt.Println("blade: built", targets, "->", ninjaFile)
	return nil
}
