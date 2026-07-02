// Package graph builds the target dependency graph from loaded BUILD files:
// it loads packages on demand, resolves dep labels to targets, enforces
// visibility, and topologically sorts the result.
package graph

import (
	"fmt"
	"path/filepath"

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
}

// NewBuilder returns a Builder that loads packages through l.
func NewBuilder(l *loader.Loader) *Builder {
	return &Builder{
		loader: l,
		loaded: map[string]bool{},
		graph:  &Graph{nodes: map[string]*Node{}},
	}
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
		dn, err := b.resolve(dlbl)
		if err != nil {
			return nil, fmt.Errorf("%s: dep %s: %w", lbl, dep, err)
		}
		if !label.VisibleTo(dn.Target.AttrStrings("visibility"), dn.Target.Package, lbl) {
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
