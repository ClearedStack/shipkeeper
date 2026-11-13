// Package health turns what was read from the cluster into the report sent
// to Muster. It is the only way out for cluster data: the report is built
// field by field from an allow-list, every string that came from the
// cluster goes through package sanitize, and the finished report is checked
// once more before it is returned.
package health

import (
	"sort"
	"strconv"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/kube"
	"github.com/ClearedStack/shipkeeper/internal/sanitize"
)

// State is a normalized health state. SPEC.md also names "idle"; it gives
// no rule for it, so this milestone never reports it.
type State string

const (
	Healthy  State = "healthy"
	Degraded State = "degraded"
	Paused   State = "paused"
)

// Report is what Muster receives about one app.
type Report struct {
	App       string   `json:"app"`
	Namespace string   `json:"namespace"`
	State     State    `json:"state"`
	Replicas  Replicas `json:"replicas"`
	Pods      []Pod    `json:"pods"`
	Events    []Event  `json:"events"`
}

type Replicas struct {
	Desired int32 `json:"desired"`
	Updated int32 `json:"updated"`
	Ready   int32 `json:"ready"`
}

// Pod is one pod, named by its position ("pod-1") and never by its name.
type Pod struct {
	Pod      string `json:"pod"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Reason   string `json:"reason,omitempty"`
	ExitCode *int32 `json:"exitCode,omitempty"`
	Message  string `json:"message,omitempty"`
}

type Event struct {
	Object  string `json:"object"` // "deployment", "replicaset" or a pod position
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Count   int32  `json:"count"`
	Message string `json:"message"`
}

// Config is what the report builder needs to know about the cluster.
type Config struct {
	ClusterDomain string
	Namespaces    []string
}

var phases = map[string]bool{"Pending": true, "Running": true, "Succeeded": true, "Failed": true}
var eventTypes = map[string]bool{"Normal": true, "Warning": true}

// Build makes the report for app from what was observed.
//
// State rules:
//
//   - paused: the Deployment asks for 0 replicas.
//   - healthy: the Deployment controller has seen the latest generation and
//     every replica asked for exists, is updated, ready and available, with
//     no replica left over from an earlier rollout.
//   - degraded: anything else.
func Build(app, namespace string, obs kube.Observed, cfg Config) (Report, error) {
	d := obs.Deployment
	if d.Spec.Replicas == nil {
		return Report{}, errs.ObservedIncomplete
	}
	want := *d.Spec.Replicas
	r := Report{
		App: app, Namespace: namespace,
		Replicas: Replicas{Desired: want, Updated: d.Status.UpdatedReplicas, Ready: d.Status.ReadyReplicas},
		Pods:     []Pod{}, Events: []Event{},
	}
	switch s := d.Status; {
	case want == 0:
		r.State = Paused
	case s.ObservedGeneration >= d.Metadata.Generation && s.Replicas == want &&
		s.UpdatedReplicas == want && s.ReadyReplicas == want && s.AvailableReplicas == want:
		r.State = Healthy
	default:
		r.State = Degraded
	}

	pods := append([]kube.Pod(nil), obs.Pods...)
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].Metadata.Name < pods[j].Metadata.Name })

	sc := sanitize.Config{ClusterDomain: cfg.ClusterDomain, Namespaces: cfg.Namespaces, Pods: map[string]string{}}
	alias := map[string]string{}
	for i, p := range pods {
		a := "pod-" + strconv.Itoa(i+1)
		alias[p.Metadata.Name] = a
		sc.Pods[p.Metadata.Name] = a
		if p.Spec.Hostname != "" {
			sc.Pods[p.Spec.Hostname] = a
		}
		sc.Nodes = append(sc.Nodes, p.Spec.NodeName, p.Status.NominatedNodeName)
		sc.Addresses = append(sc.Addresses, p.Status.HostIP, p.Status.PodIP)
		for _, a := range p.Status.HostIPs {
			sc.Addresses = append(sc.Addresses, a.IP)
		}
		for _, a := range p.Status.PodIPs {
			sc.Addresses = append(sc.Addresses, a.IP)
		}
	}
	// Node names are learned from every event in the namespace, not only
	// the ones reported, so that a name is known wherever it turns up.
	for _, e := range obs.Events {
		sc.Nodes = append(sc.Nodes, e.Source.Host, e.ReportingInstance)
		if e.InvolvedObject.Kind == "Node" {
			sc.Nodes = append(sc.Nodes, e.InvolvedObject.Name)
		}
	}
	scrub := sanitize.New(sc)

	for _, p := range pods {
		out := Pod{Pod: alias[p.Metadata.Name], Phase: "Unknown"}
		if phases[p.Status.Phase] {
			out.Phase = p.Status.Phase
		}
		var reason, message string
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				out.Ready = true
			}
			if c.Type == "PodScheduled" && c.Status == "False" {
				reason, message = c.Reason, c.Message
			}
		}
		if p.Status.Reason != "" {
			reason, message = p.Status.Reason, p.Status.Message
		}
		// The first container that is waiting or has exited explains the pod
		// better than the pod-level fields do.
		found := false
		for _, c := range p.Status.ContainerStatuses {
			out.Restarts += c.RestartCount
			if found {
				continue
			}
			if w := c.State.Waiting; w != nil {
				reason, message, found = w.Reason, w.Message, true
			} else if t := c.State.Terminated; t != nil {
				code := t.ExitCode
				reason, message, out.ExitCode, found = t.Reason, t.Message, &code, true
			}
		}
		out.Reason, out.Message = scrub.Text(reason), scrub.Text(message)
		r.Pods = append(r.Pods, out)
	}

	events := append([]kube.Event(nil), obs.Events...)
	sort.SliceStable(events, func(i, j int) bool {
		ti, tj := when(events[i]), when(events[j])
		if ti != tj {
			return ti < tj
		}
		return events[i].Metadata.Name < events[j].Metadata.Name
	})
	for _, e := range events {
		var object string
		switch o := e.InvolvedObject; {
		case o.Kind == "Deployment" && o.Name == app:
			object = "deployment"
		case o.Kind == "ReplicaSet" && strings.HasPrefix(o.Name, app+"-"):
			object = "replicaset"
		case o.Kind == "Pod" && alias[o.Name] != "":
			object = alias[o.Name]
		default:
			continue
		}
		out := Event{Object: object, Type: "Other", Count: e.Count,
			Reason: scrub.Text(e.Reason), Message: scrub.Text(e.Message)}
		if eventTypes[e.Type] {
			out.Type = e.Type
		}
		r.Events = append(r.Events, out)
	}

	// Last check. If anything the rules remove is still present in any
	// string of the report, the report is not sent at all.
	check := []string{r.App, r.Namespace}
	for _, p := range r.Pods {
		check = append(check, p.Reason, p.Message)
	}
	for _, e := range r.Events {
		check = append(check, e.Reason, e.Message)
	}
	for _, s := range check {
		if scrub.Leaks(s) {
			return Report{}, errs.ReportNotClean
		}
	}
	return r, nil
}

func when(e kube.Event) string {
	if e.LastTimestamp != "" {
		return e.LastTimestamp
	}
	return e.EventTime
}
