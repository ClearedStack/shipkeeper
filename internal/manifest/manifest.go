// Package manifest reads app.yaml and turns it into the three Kubernetes
// objects Shipkeeper applies for an app: a Deployment, a Service and a
// NetworkPolicy.
//
// SPEC.md does not define the fields of app.yaml. The format read here is a
// proposal made for the first milestone; see README.md.
package manifest

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/yamlsubset"
)

// Namespaces are the only namespaces Shipkeeper manages (SPEC.md).
var Namespaces = []string{"clearedstack", "cs-workers"}

// FieldManager is the name Shipkeeper owns fields under. It is the
// component's registry slug.
const FieldManager = "shipkeeper"

// Peer is another app allowed to connect to this one.
type Peer struct {
	Namespace string
	Name      string
}

// App is one parsed app.yaml.
type App struct {
	Name      string
	Namespace string
	Image     string // repository name inside Lazarette, without a host
	Digest    string // sha256:<64 hex>
	Replicas  int
	Port      int
	Memory    string // e.g. 32Mi
	AllowFrom []Peer // sorted
}

var (
	nameRE   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	imageRE  = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	memoryRE = regexp.MustCompile(`^[1-9][0-9]{0,4}Mi$`)
	intRE    = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})$`)
	// RegistryRE is the accepted form of the registry host Shipkeeper pulls from.
	RegistryRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[1-9][0-9]{0,4})?$`)
)

// Words that YAML 1.1 and YAML 1.2 readers type differently when unquoted.
var ambiguous = map[string]bool{
	"true": true, "false": true, "yes": true, "no": true, "on": true, "off": true,
	"y": true, "n": true, "null": true, "~": true,
}

const (
	maxReplicas  = 10
	maxAllowFrom = 16
	maxImageLen  = 128
)

var fields = []string{"name", "namespace", "image", "digest", "replicas", "port", "memory", "allowFrom"}

// Parse reads app.yaml. Every field except allowFrom is required, and a
// field that is not listed above is refused.
func Parse(data []byte) (App, error) {
	var app App
	doc, err := yamlsubset.Parse(data)
	if err != nil {
		return app, err
	}
	if doc.Kind != yamlsubset.Mapping {
		return app, errs.ManifestShape.At(doc.Line)
	}
	known := map[string]bool{}
	for _, f := range fields {
		known[f] = true
	}
	for _, k := range doc.Keys {
		if !known[k] {
			return app, errs.ManifestUnknown.At(doc.Fields[k].Line)
		}
	}
	for _, f := range fields[:len(fields)-1] {
		if doc.Fields[f] == nil {
			return app, errs.ManifestMissing
		}
	}

	if app.Name, err = text(doc.Fields["name"], nameRE); err != nil {
		return app, err
	}
	if app.Namespace, err = namespace(doc.Fields["namespace"]); err != nil {
		return app, err
	}
	if app.Image, err = text(doc.Fields["image"], imageRE); err != nil {
		return app, err
	}
	if len(app.Image) > maxImageLen {
		return app, errs.ManifestValue.At(doc.Fields["image"].Line)
	}
	if app.Digest, err = text(doc.Fields["digest"], digestRE); err != nil {
		return app, err
	}
	if app.Replicas, err = number(doc.Fields["replicas"], 0, maxReplicas); err != nil {
		return app, err
	}
	if app.Port, err = number(doc.Fields["port"], 1, 65535); err != nil {
		return app, err
	}
	if app.Memory, err = text(doc.Fields["memory"], memoryRE); err != nil {
		return app, err
	}
	if n := doc.Fields["allowFrom"]; n != nil {
		if app.AllowFrom, err = peers(n); err != nil {
			return app, err
		}
	}
	return app, nil
}

func text(n *yamlsubset.Node, re *regexp.Regexp) (string, error) {
	if n.Kind != yamlsubset.Scalar {
		return "", errs.ManifestShape.At(n.Line)
	}
	if !n.Quoted && ambiguous[strings.ToLower(n.Value)] {
		return "", errs.ManifestAmbiguous.At(n.Line)
	}
	if !re.MatchString(n.Value) {
		return "", errs.ManifestValue.At(n.Line)
	}
	return n.Value, nil
}

func namespace(n *yamlsubset.Node) (string, error) {
	v, err := text(n, nameRE)
	if err != nil {
		return "", err
	}
	for _, ns := range Namespaces {
		if v == ns {
			return v, nil
		}
	}
	return "", errs.ManifestNamespace.At(n.Line)
}

func number(n *yamlsubset.Node, min, max int) (int, error) {
	if n.Kind != yamlsubset.Scalar {
		return 0, errs.ManifestShape.At(n.Line)
	}
	if n.Quoted || !intRE.MatchString(n.Value) {
		return 0, errs.ManifestValue.At(n.Line)
	}
	v, _ := strconv.Atoi(n.Value)
	if v < min || v > max {
		return 0, errs.ManifestValue.At(n.Line)
	}
	return v, nil
}

func peers(n *yamlsubset.Node) ([]Peer, error) {
	if n.Kind != yamlsubset.Sequence {
		return nil, errs.ManifestShape.At(n.Line)
	}
	if len(n.Items) > maxAllowFrom {
		return nil, errs.ManifestValue.At(n.Line)
	}
	var out []Peer
	seen := map[Peer]bool{}
	for _, item := range n.Items {
		ns, name, ok := strings.Cut(item.Value, "/")
		if !ok || !nameRE.MatchString(ns) || !nameRE.MatchString(name) {
			return nil, errs.ManifestValue.At(item.Line)
		}
		managed := false
		for _, m := range Namespaces {
			managed = managed || ns == m
		}
		if !managed {
			return nil, errs.ManifestNamespace.At(item.Line)
		}
		p := Peer{ns, name}
		if seen[p] {
			return nil, errs.ManifestValue.At(item.Line)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

type m = map[string]any

const (
	nameLabel    = "app.kubernetes.io/name"
	managedLabel = "app.kubernetes.io/managed-by"
)

// SelectorLabel is the label Shipkeeper selects an app's pods by.
const SelectorLabel = nameLabel

// Render returns the NetworkPolicy, Service and Deployment for app, in the
// order they are applied. registry is the host[:port] images are pulled
// from. The same App and registry always give the same bytes.
func Render(app App, registry string) ([]kube.Object, error) {
	if !RegistryRE.MatchString(registry) {
		return nil, errs.Config
	}
	labels := m{nameLabel: app.Name, managedLabel: FieldManager}
	selector := m{nameLabel: app.Name}
	meta := m{"name": app.Name, "namespace": app.Namespace, "labels": labels}

	from := []any{}
	for _, p := range app.AllowFrom {
		from = append(from, m{
			"namespaceSelector": m{"matchLabels": m{"kubernetes.io/metadata.name": p.Namespace}},
			"podSelector":       m{"matchLabels": m{nameLabel: p.Name}},
		})
	}
	ingress := []any{}
	if len(from) > 0 {
		ingress = append(ingress, m{
			"from":  from,
			"ports": []any{m{"port": app.Port, "protocol": "TCP"}},
		})
	}
	policy := m{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta,
		"spec": m{
			"podSelector": m{"matchLabels": selector},
			"policyTypes": []any{"Ingress"},
			"ingress":     ingress,
		},
	}
	service := m{
		"apiVersion": "v1", "kind": "Service", "metadata": meta,
		"spec": m{
			"type":     "ClusterIP",
			"selector": selector,
			"ports":    []any{m{"port": app.Port, "targetPort": app.Port, "protocol": "TCP"}},
		},
	}
	deployment := m{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta,
		"spec": m{
			"replicas": app.Replicas,
			"selector": m{"matchLabels": selector},
			"template": m{
				"metadata": m{"labels": labels},
				"spec": m{
					"securityContext": m{"runAsNonRoot": true},
					"containers": []any{m{
						"name":  app.Name,
						"image": registry + "/" + app.Image + "@" + app.Digest,
						"ports": []any{m{"containerPort": app.Port, "protocol": "TCP"}},
						"resources": m{
							"requests": m{"memory": app.Memory},
							"limits":   m{"memory": app.Memory},
						},
						"readinessProbe": m{"tcpSocket": m{"port": app.Port}},
						"securityContext": m{
							"allowPrivilegeEscalation": false,
							"readOnlyRootFilesystem":   true,
							"capabilities":             m{"drop": []any{"ALL"}},
						},
					}},
				},
			},
		},
	}

	var out []kube.Object
	for _, o := range []m{policy, service, deployment} {
		body, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			return nil, errs.Unexpected
		}
		out = append(out, kube.Object{
			APIVersion: o["apiVersion"].(string), Kind: o["kind"].(string),
			Namespace: app.Namespace, Name: app.Name, Body: append(body, '\n'),
		})
	}
	return out, nil
}
