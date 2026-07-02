// Package graph builds the target dependency graph from loaded BUILD files:
// it loads packages on demand, resolves dep labels to targets, enforces
// visibility, and topologically sorts the result.
package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blade-build/blade-go/internal/label"
	"github.com/blade-build/blade-go/internal/loader"
	"github.com/blade-build/blade-go/internal/target"
)

// Node is a target plus its resolved dependencies.
type Node struct {
	Target  *target.Target
	Deps    []*Node          // resolved non-syslib dependencies
	Syslibs []label.Label    // "#name" system-library dependencies
	Vcpkgs  []label.VcpkgDep // "vcpkg#port:lib" thirdparty dependencies
}

// Label returns the node's canonical label.
func (n *Node) Label() string { return n.Target.Label() }

// Graph is the resolved dependency graph.
type Graph struct {
	nodes map[string]*Node
	order []*Node // node creation order
}

// Node returns the node for a label, or nil.
func (g *Graph) Node(labelStr string) *Node { return g.nodes[labelStr] }

// Len returns the number of nodes.
func (g *Graph) Len() int { return len(g.order) }

// All returns the nodes in creation order.
func (g *Graph) All() []*Node { return append([]*Node(nil), g.order...) }

// Builder expands a dependency graph from root labels, loading each referenced
// package's BUILD file at most once.
type Builder struct {
	loader *loader.Loader
	loaded map[string]bool
	graph  *Graph

	// VcpkgPrefix routes deps whose package starts with this prefix to vcpkg
	// instead of loading their BUILD (e.g. "thirdparty/" -> vcpkg#<port>:<lib>).
	// Empty disables the mapping.
	VcpkgPrefix string

	// legacyPublic holds the "pkg:name" keys that global_config's
	// legacy_public_targets marks PUBLIC by default (blade's grandfather list for
	// targets that predate explicit visibility). Populated lazily from config.
	legacyPublic map[string]bool
}

// NewBuilder returns a Builder that loads packages through l, mapping
// //thirdparty/... deps to vcpkg by default.
func NewBuilder(l *loader.Loader) *Builder {
	return &Builder{
		loader:       l,
		loaded:       map[string]bool{},
		graph:        &Graph{nodes: map[string]*Node{}},
		VcpkgPrefix:  "thirdparty/",
		legacyPublic: legacyPublicSet(l),
	}
}

// legacyPublicSet reads global_config's legacy_public_targets into a set of
// "pkg:name" keys.
func legacyPublicSet(l *loader.Loader) map[string]bool {
	set := map[string]bool{}
	if l == nil || l.Config == nil {
		return set
	}
	if v, ok := l.Config.GetItem("global_config", "legacy_public_targets"); ok {
		if list, ok := v.([]any); ok {
			for _, e := range list {
				if s, ok := e.(string); ok {
					set[s] = true
				}
			}
		}
	}
	return set
}

// vcpkgFromThirdparty maps a thirdparty label to a vcpkg dep: the port is the
// first path component after the prefix, the lib is the target name. Returns
// false when the label is not under the prefix.
// hasRealTarget reports whether the label names a target defined by a real BUILD
// file (as opposed to a bare //thirdparty/<port>:<lib> vcpkg reference). Used to
// let source-built thirdparty packages (jsoncpp) win over the vcpkg heuristic.
func (b *Builder) hasRealTarget(lbl label.Label) bool {
	buildFile := filepath.Join(b.loader.Root, filepath.FromSlash(lbl.Package), "BUILD")
	if _, err := os.Stat(buildFile); err != nil {
		return false
	}
	if err := b.ensurePackage(lbl.Package); err != nil {
		return false
	}
	return b.loader.Targets.Get(lbl.String()) != nil
}

func (b *Builder) vcpkgFromThirdparty(lbl label.Label) (label.VcpkgDep, bool) {
	if b.VcpkgPrefix == "" || !strings.HasPrefix(lbl.Package, b.VcpkgPrefix) {
		return label.VcpkgDep{}, false
	}
	rest := strings.TrimPrefix(lbl.Package, b.VcpkgPrefix)
	port := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		port = rest[:i]
	}
	return label.VcpkgDep{Port: port, Lib: lbl.Name}, true
}

// Build resolves the transitive closure of the given root labels and returns the
// graph.
func (b *Builder) Build(roots []string) (*Graph, error) {
	for _, r := range roots {
		lbl, err := label.Parse(r, "")
		if err != nil {
			return nil, err
		}
		if _, err := b.resolve(lbl); err != nil {
			return nil, err
		}
	}
	return b.graph, nil
}

func (b *Builder) ensurePackage(pkg string) error {
	if b.loaded[pkg] {
		return nil
	}
	b.loaded[pkg] = true
	buildFile := filepath.Join(b.loader.Root, filepath.FromSlash(pkg), "BUILD")
	if err := b.loader.LoadBuildFile(buildFile); err != nil {
		return fmt.Errorf("loading package %q: %w", pkg, err)
	}
	return nil
}

func (b *Builder) resolve(lbl label.Label) (*Node, error) {
	if n, ok := b.graph.nodes[lbl.String()]; ok {
		return n, nil
	}
	if err := b.ensurePackage(lbl.Package); err != nil {
		return nil, err
	}
	tgt := b.loader.Targets.Get(lbl.String())
	if tgt == nil {
		return nil, fmt.Errorf("no such target %s", lbl)
	}
	n := &Node{Target: tgt}
	// Register before resolving deps so a dependency cycle links back instead of
	// recursing forever; TopoSort reports the cycle.
	b.graph.nodes[lbl.String()] = n
	b.graph.order = append(b.graph.order, n)

	for _, dep := range tgt.AttrStrings("deps") {
		if label.IsVcpkg(dep) {
			n.Vcpkgs = append(n.Vcpkgs, label.ParseVcpkg(dep))
			continue
		}
		dlbl, err := label.Parse(dep, tgt.Package)
		if err != nil {
			return nil, fmt.Errorf("%s: dep %q: %w", lbl, dep, err)
		}
		if dlbl.IsSyslib() {
			n.Syslibs = append(n.Syslibs, dlbl)
			continue
		}
		if v, ok := b.vcpkgFromThirdparty(dlbl); ok && !b.hasRealTarget(dlbl) {
			// //thirdparty/<port>:<lib> with no BUILD of its own is a vcpkg port.
			// But a source-built thirdparty package (jsoncpp) has a real BUILD;
			// its own gen_rule chain deps must resolve to those targets, not be
			// misrouted to a vcpkg port that shares the directory name.
			n.Vcpkgs = append(n.Vcpkgs, v)
			continue
		}
		dn, err := b.resolve(dlbl)
		if err != nil {
			return nil, fmt.Errorf("%s: dep %s: %w", lbl, dep, err)
		}
		vis := dn.Target.AttrStrings("visibility")
		legacyPub := len(vis) == 0 && b.legacyPublic[dn.Target.Package+":"+dn.Target.Name]
		if !legacyPub && !label.VisibleTo(vis, dn.Target.Package, lbl) {
			return nil, fmt.Errorf("%s depends on %s, which is not visible to it", lbl, dlbl)
		}
		n.Deps = append(n.Deps, dn)
	}
	return n, nil
}

// TopoSort returns the nodes with every node ordered after all of its
// dependencies, erroring on a dependency cycle.
func (g *Graph) TopoSort() ([]*Node, error) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[*Node]int, len(g.order))
	var order []*Node
	var visit func(n *Node) error
	visit = func(n *Node) error {
		switch color[n] {
		case gray:
			return fmt.Errorf("dependency cycle through %s", n.Label())
		case black:
			return nil
		}
		color[n] = gray
		for _, d := range n.Deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		color[n] = black
		order = append(order, n)
		return nil
	}
	for _, n := range g.order {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}
