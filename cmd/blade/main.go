// Command blade is a Go reimplementation of the Blade build system.
//
// Work in progress: the goal is to build the C++ RPC framework "flare" with
// sufficient tests and coverage. See README.md for the phased plan.
package main

import (
	"fmt"
	"os"
	"runtime/pprof"

	"github.com/blade-build/blade-go/internal/build"
	"github.com/blade-build/blade-go/internal/resource"
	"github.com/blade-build/blade-go/internal/version"
)

func main() {
	if p := os.Getenv("BLADE_CPUPROFILE"); p != "" {
		f, _ := os.Create(p)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	// Hidden codegen subcommands invoked from generated ninja edges
	// (resource_library), mirroring blade's builtin_command architecture.
	if len(os.Args) >= 2 && (os.Args[1] == "__gen-resource" || os.Args[1] == "__gen-resource-index") {
		if err := runResourceGen(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "blade: "+err.Error())
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && (os.Args[1] == "build" || os.Args[1] == "test") {
		run := runBuild
		if os.Args[1] == "test" {
			run = runTest
		}
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "blade: "+err.Error())
			os.Exit(1)
		}
		return
	}
	fmt.Printf("blade-go %s (reimplementation in progress)\n", version.Version)
}

// runResourceGen dispatches the resource_library codegen subcommands.
//
//	__gen-resource       <out.c> <in>
//	__gen-resource-index <name> <path> <hdr> <src.c> <resource>...
func runResourceGen(cmd string, args []string) error {
	if cmd == "__gen-resource" {
		if len(args) != 2 {
			return fmt.Errorf("__gen-resource: want <out> <in>")
		}
		return resource.GenerateResource(args[1], args[0])
	}
	if len(args) < 4 {
		return fmt.Errorf("__gen-resource-index: want <name> <path> <hdr> <src> [resource...]")
	}
	return resource.GenerateIndex(args[0], args[1], args[2], args[3], args[4:])
}

func runTest(targets []string) error {
	if len(targets) == 0 {
		return fmt.Errorf("usage: blade test <target>...")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := build.FindRoot(cwd)
	if err != nil {
		return err
	}
	results, err := build.Test(root, targets)
	if err != nil {
		return err
	}
	passed := 0
	for _, r := range results {
		status := "FAIL"
		if r.Passed {
			passed++
			status = "PASS"
		}
		fmt.Printf("%s %s\n", status, r.Label)
		if !r.Passed {
			fmt.Print(r.Output)
		}
	}
	fmt.Printf("blade: %d/%d tests passed\n", passed, len(results))
	if passed != len(results) {
		return fmt.Errorf("%d test(s) failed", len(results)-passed)
	}
	return nil
}

func runBuild(args []string) error {
	// --no-build: run the front-end (load + graph + generate ninja) but don't run
	// ninja -- for timing/inspection, mirroring Python blade's flag.
	noBuild := false
	var targets []string
	for _, a := range args {
		if a == "--no-build" {
			noBuild = true
			continue
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		return fmt.Errorf("usage: blade build [--no-build] <target>...")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := build.FindRoot(cwd)
	if err != nil {
		return err
	}
	ninjaFile, err := build.Build(root, targets, build.Options{RunNinja: !noBuild})
	if err != nil {
		return err
	}
	verb := "built"
	if noBuild {
		verb = "generated"
	}
	fmt.Println("blade:", verb, targets, "->", ninjaFile)
	return nil
}
