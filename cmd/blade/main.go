// Command blade is a Go reimplementation of the Blade build system.
//
// The command line mirrors Python Blade (argparse) so it can stand in as a
// drop-in: the same `blade build|test [flags] targets...` shape, GNU long/short
// flags, and the common flags (-p, -j, -k, -n, --stop-after, --no-build).
// Flags blade-go doesn't implement yet are tolerated (accepted and ignored)
// rather than rejected, so an existing Blade command line still runs.
package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"strconv"

	"github.com/spf13/cobra"

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
	// (resource_library), mirroring blade's builtin_command architecture. Handled
	// before cobra so they aren't part of the user-facing command tree.
	if len(os.Args) >= 2 && (os.Args[1] == "__gen-resource" || os.Args[1] == "__gen-resource-index") {
		if err := runResourceGen(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "blade: "+err.Error())
			os.Exit(1)
		}
		return
	}
	if err := newRootCmd().Execute(); err != nil {
		// cobra already prints the error + usage; just set the exit code.
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "blade <command> [flags] [targets...]",
		Short:         "A Go reimplementation of the Blade build system",
		Version:       version.Version,
		SilenceUsage:  true, // don't dump usage on a runtime (non-flag) error
		SilenceErrors: false,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(newBuildCmd(), newTestCmd())
	return root
}

// buildFlags are the Blade-compatible flags a build/test accepts. Only the ones
// blade-go can honor take effect; the rest are recorded and ignored.
type buildFlags struct {
	jobs      int
	keepGoing bool
	dryRun    bool
	noBuild   bool
	stopAfter string
	profile   string
}

// register adds the flags to fs and tolerates unknown Blade flags.
func (bf *buildFlags) register(c *cobra.Command) {
	f := c.Flags()
	f.IntVarP(&bf.jobs, "jobs", "j", 0, "number of parallel build jobs (ninja -j)")
	f.BoolVarP(&bf.keepGoing, "keep-going", "k", false, "keep building after errors (ninja -k 0)")
	f.BoolVarP(&bf.dryRun, "dry-run", "n", false, "don't run build commands (ninja -n)")
	f.BoolVar(&bf.noBuild, "no-build", false, "generate build.ninja but don't run ninja")
	f.StringVar(&bf.stopAfter, "stop-after", "", "stop after a phase: load|analyze|generate|build")
	f.StringVarP(&bf.profile, "profile", "p", "release", "build profile: release|debug")

	// Blade's boolean (store_true) flags that blade-go doesn't act on: declare
	// them (hidden) so they parse as booleans and don't swallow a following
	// target the way an unknown flag would; value-taking Blade flags are handled
	// by the UnknownFlags whitelist below (they correctly consume their value).
	for _, name := range []string{
		"verbose", "quiet", "coverage", "gcov", "gprof", "fission", "dwp", "force",
		"full-test", "no-test", "generate-dynamic", "generate-java", "generate-php",
		"generate-python", "generate-go", "generate-package", "cc-check-undefined",
		"no-cc-check-undefined", "load-local-config", "no-load-local-config",
		"profiling", "autofdo-generate", "run-unrepaired-tests", "show-details",
		"all-tags", "no-debug-info",
	} {
		var ignored bool
		f.BoolVar(&ignored, name, false, "(accepted for Blade compatibility; ignored)")
		_ = f.MarkHidden(name)
	}
	// Accept (and ignore) any remaining Blade flags blade-go doesn't implement,
	// so an existing Blade command line still runs instead of erroring.
	c.FParseErrWhitelist.UnknownFlags = true
}

// runNinja reports whether ninja should run, and the extra ninja args, from the
// parsed flags. --no-build or --stop-after {load,analyze,generate} => front-end
// only (as in Blade).
func (bf *buildFlags) ninja() (run bool, args []string) {
	run = true
	switch bf.stopAfter {
	case "load", "analyze", "generate":
		run = false
	}
	if bf.noBuild {
		run = false
	}
	if bf.jobs > 0 {
		args = append(args, "-j", strconv.Itoa(bf.jobs))
	}
	if bf.keepGoing {
		args = append(args, "-k", "0")
	}
	if bf.dryRun {
		args = append(args, "-n")
	}
	return run, args
}

func newBuildCmd() *cobra.Command {
	var bf buildFlags
	c := &cobra.Command{
		Use:   "build [flags] <target>...",
		Short: "Build the given targets (or patterns like //pkg/...)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, targets []string) error {
			root, err := workspaceRoot()
			if err != nil {
				return err
			}
			run, nargs := bf.ninja()
			ninjaFile, err := build.Build(root, targets, build.Options{RunNinja: run, NinjaArgs: nargs})
			if err != nil {
				return err
			}
			verb := "built"
			if !run {
				verb = "generated"
			}
			fmt.Println("blade:", verb, targets, "->", ninjaFile)
			return nil
		},
	}
	bf.register(c)
	return c
}

func newTestCmd() *cobra.Command {
	var bf buildFlags
	c := &cobra.Command{
		Use:   "test [flags] <target>...",
		Short: "Build and run the cc_test targets in the given patterns",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, targets []string) error {
			root, err := workspaceRoot()
			if err != nil {
				return err
			}
			_, nargs := bf.ninja()
			results, err := build.Test(root, targets, build.Options{NinjaArgs: nargs})
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
		},
	}
	bf.register(c)
	return c
}

func workspaceRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return build.FindRoot(cwd)
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
