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
	"github.com/blade-build/blade-go/internal/hdrcheck"
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
	root.AddCommand(newBuildCmd(), newTestCmd(), newRunCmd(), newCleanCmd(), newQueryCmd(), newVersionCmd())
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
	hdrCheck  string // header inclusion check: off|warn|error
}

// register adds the flags to fs and tolerates unknown Blade flags.
func (bf *buildFlags) register(c *cobra.Command) {
	f := c.Flags()
	f.IntVarP(&bf.jobs, "jobs", "j", 0, "parallel build jobs / ninja -j (0 = CPUs available to the process, cgroup-aware; set high for distributed builds)")
	f.BoolVarP(&bf.keepGoing, "keep-going", "k", false, "keep building after errors (ninja -k 0)")
	f.BoolVarP(&bf.dryRun, "dry-run", "n", false, "don't run build commands (ninja -n)")
	f.BoolVar(&bf.noBuild, "no-build", false, "generate build.ninja but don't run ninja")
	f.StringVar(&bf.stopAfter, "stop-after", "", "stop after a phase: load|analyze|generate|build")
	f.StringVarP(&bf.profile, "profile", "p", "release", "build profile: release|debug")
	f.StringVar(&bf.hdrCheck, "hdr-check", "", "header inclusion-dependency check: off|warn|error (default: project cc_config)")

	// Blade's boolean (store_true) flags that blade-go doesn't act on: declare
	// them (hidden) so they parse as booleans and don't swallow a following
	// target the way an unknown flag would; value-taking Blade flags are handled
	// by the UnknownFlags whitelist below (they correctly consume their value).
	for _, name := range []string{
		"verbose", "quiet", "coverage", "gcov", "gprof", "fission", "dwp", "force",
		"no-test", "generate-dynamic", "generate-java", "generate-php",
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
	// Always pass an explicit -j. When unset, use the cgroup-aware CPU count
	// rather than letting ninja pick: ninja's default respects a cpuset but not a
	// CFS quota (docker --cpus=N), so it would over-parallelize in that common
	// container case and risk OOM. An explicit -j (e.g. large, for distributed
	// builds) overrides this.
	jobs := bf.jobs
	if jobs <= 0 {
		jobs = build.DefaultJobs()
	}
	args = append(args, "-j", strconv.Itoa(jobs))
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
			// The header check reads the `.d` depfiles a build produces, so run
			// it only when we actually built (not --no-build / --stop-after).
			if run {
				return runHdrCheck(root, targets, bf.hdrCheck)
			}
			return nil
		},
	}
	bf.register(c)
	return c
}

// runHdrCheck runs the header inclusion-dependency check and reports issues.
// The severity comes from `override` (a --hdr-check value) or, when empty, the
// project's cc_config. Returns an error (failing the command) only when the
// effective severity is "error" and issues were found; "warn" prints and returns
// nil; "off" is a no-op.
func runHdrCheck(root string, targets []string, override string) error {
	issues, sev, err := build.CheckHdrs(root, targets, override)
	if err != nil {
		return err
	}
	if len(issues) == 0 || sev == hdrcheck.Off {
		return nil
	}
	sevWord := "warning"
	if sev == hdrcheck.Error {
		sevWord = "error"
	}
	for _, is := range issues {
		fmt.Fprintln(os.Stderr, is.Format(sevWord))
	}
	fmt.Fprintf(os.Stderr, "blade: hdr-check found %d issue(s)\n", len(issues))
	if sev == hdrcheck.Error {
		return fmt.Errorf("header check failed: %d issue(s)", len(issues))
	}
	return nil
}

func newTestCmd() *cobra.Command {
	var bf buildFlags
	var fullTest bool
	var testJobs int
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
			// Stream each result as it finishes so a large suite shows progress
			// instead of looking hung until the last (possibly slow) test ends.
			print := func(r build.TestResult) {
				status := "FAIL"
				if r.Passed {
					status = "PASS"
				}
				suffix := ""
				if r.Cached {
					suffix = " (cached)" // passed last time, inputs unchanged
				}
				fmt.Printf("%s %s%s\n", status, r.Label, suffix)
				if !r.Passed {
					fmt.Print(r.Output)
				}
			}
			results, err := build.Test(root, targets, build.Options{NinjaArgs: nargs, FullTest: fullTest, TestJobs: testJobs}, print)
			if err != nil {
				return err
			}
			passed := 0
			for _, r := range results {
				if r.Passed {
					passed++
				}
			}
			cached := 0
			for _, r := range results {
				if r.Cached {
					cached++
				}
			}
			note := ""
			if cached > 0 {
				note = fmt.Sprintf(" (%d cached)", cached)
			}
			fmt.Printf("blade: %d/%d tests passed%s\n", passed, len(results), note)
			// The header check is part of the build, so `blade test` -- which builds
			// its targets -- runs it too. A test failure takes precedence in the exit
			// code, but the check still reports either way.
			hdrErr := runHdrCheck(root, targets, bf.hdrCheck)
			if passed != len(results) {
				return fmt.Errorf("%d test(s) failed", len(results)-passed)
			}
			return hdrErr
		},
	}
	bf.register(c)
	c.Flags().BoolVar(&fullTest, "full-test", false, "re-run every test, ignoring the incremental cache")
	c.Flags().IntVar(&testJobs, "test-jobs", 0, "parallel test workers (0 = CPUs available to the process, cgroup-aware; exclusive tests always run serially)")
	return c
}

func newRunCmd() *cobra.Command {
	var bf buildFlags
	c := &cobra.Command{
		Use:   "run [flags] <target> [-- args...]",
		Short: "Build a single target and run it (args after -- go to the program)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Split targets from program args at the "--" separator.
			target, progArgs := args[0], []string{}
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				target = args[dash-1]
				progArgs = args[dash:]
			}
			root, err := workspaceRoot()
			if err != nil {
				return err
			}
			_, nargs := bf.ninja()
			code, err := build.Run(root, target, progArgs, build.Options{NinjaArgs: nargs})
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	bf.register(c)
	return c
}

func newQueryCmd() *cobra.Command {
	var deps, dependents, depended bool
	c := &cobra.Command{
		Use:   "query [--deps | --dependents] <target>...",
		Short: "Show a target's transitive dependencies (--deps) or dependents",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, targets []string) error {
			reverse := dependents || depended
			// --deps is the default when neither direction is given.
			root, err := workspaceRoot()
			if err != nil {
				return err
			}
			results, err := build.Query(root, targets, reverse)
			if err != nil {
				return err
			}
			arrow := "depends on"
			if reverse {
				arrow = "depended on by"
			}
			for _, r := range results {
				fmt.Printf("%s %s:\n", r.Target, arrow)
				for _, d := range r.Related {
					fmt.Printf("  %s\n", d)
				}
				if len(r.Related) == 0 {
					fmt.Println("  (none)")
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&deps, "deps", false, "show transitive dependencies (default)")
	c.Flags().BoolVar(&dependents, "dependents", false, "show transitive dependents (reverse)")
	c.Flags().BoolVar(&depended, "depended", false, "alias of --dependents")
	c.FParseErrWhitelist.UnknownFlags = true
	return c
}

func newCleanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "clean",
		Short: "Remove the build output directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := workspaceRoot()
			if err != nil {
				return err
			}
			if err := build.Clean(root); err != nil {
				return err
			}
			fmt.Println("blade: cleaned")
			return nil
		},
	}
	c.FParseErrWhitelist.UnknownFlags = true // tolerate Blade's clean flags
	return c
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the blade-go version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("blade-go %s\n", version.Version)
			return nil
		},
	}
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
