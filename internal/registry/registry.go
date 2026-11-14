// Package registry lists the seventeen ClearedStack platform components from
// the platform naming registry: seven in clearedstack and ten in cs-workers.
package registry

// Kind distinguishes control-plane services from deterministic workers.
type Kind string

const (
	ControlPlane Kind = "control-plane"
	Worker       Kind = "worker"
)

// Namespaces managed by Shipkeeper.
const (
	ControlNamespace = "clearedstack"
	WorkerNamespace  = "cs-workers"
)

// Shipkeeper is this component's slug in the naming registry.
const Shipkeeper = "shipkeeper"

// Component is one entry from the ClearedStack naming registry.
type Component struct {
	Slug       string
	Kind       Kind
	Namespace  string
	Repository string
	Image      string
	Stage      int // 1..10 for workers, 0 for control-plane services
}

var components = []Component{
	{Slug: "conn", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "clearedstack.com", Image: "conn"},
	{Slug: "muster", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "muster", Image: "muster"},
	{Slug: "logbook", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "logbook", Image: "postgres"},
	{Slug: "plainforge", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "plainforge", Image: "plainforge"},
	{Slug: "lazarette", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "lazarette", Image: "lazarette"},
	{Slug: "keelworks", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "keelworks", Image: "keelworks"},
	{Slug: "shipkeeper", Kind: ControlPlane, Namespace: ControlNamespace, Repository: "shipkeeper", Image: "shipkeeper"},
	{Slug: "trawl", Kind: Worker, Namespace: WorkerNamespace, Repository: "trawl", Image: "trawl", Stage: 1},
	{Slug: "sextant", Kind: Worker, Namespace: WorkerNamespace, Repository: "sextant", Image: "sextant", Stage: 2},
	{Slug: "assay", Kind: Worker, Namespace: WorkerNamespace, Repository: "assay", Image: "assay", Stage: 3},
	{Slug: "countersign", Kind: Worker, Namespace: WorkerNamespace, Repository: "countersign", Image: "countersign", Stage: 4},
	{Slug: "waybill", Kind: Worker, Namespace: WorkerNamespace, Repository: "waybill", Image: "waybill", Stage: 5},
	{Slug: "moldloft", Kind: Worker, Namespace: WorkerNamespace, Repository: "moldloft", Image: "moldloft", Stage: 6},
	{Slug: "shakedown", Kind: Worker, Namespace: WorkerNamespace, Repository: "shakedown", Image: "shakedown", Stage: 7},
	{Slug: "chartroom", Kind: Worker, Namespace: WorkerNamespace, Repository: "chartroom", Image: "chartroom", Stage: 8},
	{Slug: "tidegauge", Kind: Worker, Namespace: WorkerNamespace, Repository: "tidegauge", Image: "tidegauge", Stage: 9},
	{Slug: "bellbuoy", Kind: Worker, Namespace: WorkerNamespace, Repository: "bellbuoy", Image: "bellbuoy", Stage: 10},
}

var bySlug = func() map[string]Component {
	m := make(map[string]Component, len(components))
	for _, c := range components {
		m[c.Slug] = c
	}
	return m
}()

// All returns the seventeen components in registry order: control-plane
// services first, then workers by pipeline stage.
func All() []Component {
	out := make([]Component, len(components))
	copy(out, components)
	return out
}

// Lookup finds a component by its registry slug.
func Lookup(slug string) (Component, bool) {
	c, ok := bySlug[slug]
	return c, ok
}

// IsWorker reports whether slug names one of the ten workers in cs-workers.
func IsWorker(slug string) bool {
	c, ok := bySlug[slug]
	return ok && c.Kind == Worker
}
