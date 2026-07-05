// Package windef generates a Windows module-definition (.def) file that
// auto-exports the symbols of a set of object files -- the blade-go analog of
// Blade's cc_windef tool. blade-go invokes it via the hidden `__gen-windef`
// subcommand from a generated ninja edge, so a cc_library built as a DLL exports
// its symbols without every source needing __declspec(dllexport).
package windef

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Generate writes a .def exporting every defined external symbol of objs. It
// reads each object's COFF symbol table with `dumpbin /SYMBOLS` (run in the MSVC
// dev environment this process inherits from ninja). A symbol whose dumpbin line
// lacks the "()" function marker is exported as DATA (so the consumer's import
// resolves to the datum, not a bogus function). Symbols are de-duplicated by name
// (COMDAT/template instantiations appear in several objects). Written
// write-if-changed so an unchanged export set keeps the .def's mtime, letting
// ninja's restat prune the DLL relink.
func Generate(out string, objs []string) error {
	type exp struct {
		name   string
		isData bool
	}
	seen := map[string]bool{}
	var exports []exp
	for _, obj := range objs {
		o, err := exec.Command("dumpbin", "/NOLOGO", "/SYMBOLS", obj).Output()
		if err != nil {
			return fmt.Errorf("dumpbin %s: %w", obj, err)
		}
		for _, raw := range strings.Split(string(o), "\n") {
			ext := strings.Index(raw, " External ")
			if ext < 0 || strings.Contains(raw[:ext], "UNDEF") {
				continue // not an external, or undefined (imported, not exported)
			}
			bar := strings.LastIndexByte(raw, '|')
			if bar < 0 || bar < ext {
				continue
			}
			name := strings.TrimSpace(raw[bar+1:])
			// dumpbin appends " (demangled signature)"; the real symbol is the
			// first whitespace-delimited token.
			if sp := strings.IndexByte(name, ' '); sp >= 0 {
				name = name[:sp]
			}
			if name == "" || !exportable(name) || seen[name] {
				continue
			}
			seen[name] = true
			// The "()" after the type marks a function; its absence -> data.
			exports = append(exports, exp{name, !strings.Contains(raw[:bar], "()")})
		}
	}
	var b strings.Builder
	b.WriteString("EXPORTS\r\n")
	for _, e := range exports {
		if e.isData {
			fmt.Fprintf(&b, "    %s DATA\r\n", e.name)
		} else {
			fmt.Fprintf(&b, "    %s\r\n", e.name)
		}
	}
	return writeIfChanged(out, b.String())
}

// exportable drops compiler-internal symbols that must never be exported: string
// / FP / SIMD literal COMDATs (??_C@, __real@, __xmm@) and the CRT entry points.
func exportable(name string) bool {
	for _, p := range []string{"??_C@", "__real@", "__xmm@", "__ymm@"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	switch name {
	case "main", "wmain", "WinMain", "wWinMain", "DllMain",
		"mainCRTStartup", "wmainCRTStartup", "_DllMainCRTStartup":
		return false
	}
	return true
}

func writeIfChanged(path, content string) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if _, err := w.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
