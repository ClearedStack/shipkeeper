// Package kubetest is an in-memory stand-in for the Kubernetes API, used as
// the test cluster. It speaks just enough of the real HTTP API for
// Shipkeeper: server-side apply of Deployments, Services and
// NetworkPolicies with field ownership and conflicts, reading a Deployment,
// and listing pods and events.
//
// It is a simplification written for these tests, not a copy of the real
// server. Differences that matter: a list is owned as one field (the real
// server tracks list entries one by one), nothing is defaulted or
// validated, and no controller runs, so pods, events and Deployment status
// appear only when a test puts them there.
package kubetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Request is one request the server received.
type Request struct {
	Method string
	Path   string
	DryRun bool
}

type object struct {
	data       map[string]any
	owners     map[string]map[string]bool // field path -> managers
	generation int64
	status     any
}

// Server is the stand-in API server.
type Server struct {
	URL string

	mu         sync.Mutex
	http       *httptest.Server
	namespaces map[string]bool
	objects    map[string]*object           // "<collection path>/<name>"
	pods       map[string][]json.RawMessage // namespace -> pods
	events     map[string][]json.RawMessage // namespace -> events
	requests   []Request

	// Failure switches, set by tests.
	Token   string        // when set, requests must carry this bearer token
	Delay   time.Duration // wait this long before answering
	Garbage bool          // answer every GET with text that is not JSON
}

// New starts a server that has the given namespaces.
func New(namespaces ...string) *Server {
	s := &Server{
		namespaces: map[string]bool{},
		objects:    map[string]*object{},
		pods:       map[string][]json.RawMessage{},
		events:     map[string][]json.RawMessage{},
	}
	for _, ns := range namespaces {
		s.namespaces[ns] = true
	}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.http.URL
	return s
}

// Close stops the server.
func (s *Server) Close() { s.http.Close() }

// Requests returns what the server has received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Writes counts the applies that were not dry runs.
func (s *Server) Writes() int {
	n := 0
	for _, r := range s.Requests() {
		if r.Method == http.MethodPatch && !r.DryRun {
			n++
		}
	}
	return n
}

// AddPod and AddEvent put a raw object into a namespace, as a kubelet or a
// controller would.
func (s *Server) AddPod(namespace string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pods[namespace] = append(s.pods[namespace], raw)
}

func (s *Server) AddEvent(namespace string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[namespace] = append(s.events[namespace], raw)
}

// SetDeploymentStatus sets the status block returned for a Deployment.
func (s *Server) SetDeploymentStatus(namespace, name string, status any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.objects[deployments(namespace)+"/"+name]; o != nil {
		o.status = status
	}
}

// RollOut marks a Deployment as fully rolled out at its current generation.
func (s *Server) RollOut(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[deployments(namespace)+"/"+name]
	if o == nil {
		return
	}
	n, _ := get(o.data, dotted("spec.replicas")).(float64)
	o.status = map[string]any{
		"observedGeneration": o.generation, "replicas": n, "updatedReplicas": n,
		"readyReplicas": n, "availableReplicas": n,
	}
}

// Edit changes one field of a Deployment as some other field manager, the
// way a manual out-of-band edit does. That manager becomes the field's only
// owner.
func (s *Server) Edit(namespace, name, manager, path string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[deployments(namespace)+"/"+name]
	if o == nil {
		return
	}
	raw, _ := json.Marshal(value) // store it the way a decoded request would be
	json.Unmarshal(raw, &value)
	set(o.data, dotted(path), value)
	o.owners[dotted(path)] = map[string]bool{manager: true}
	if strings.HasPrefix(path, "spec.") {
		o.generation++
	}
}

// Field returns one field of a stored Deployment, or nil.
func (s *Server) Field(namespace, name, path string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.objects[deployments(namespace)+"/"+name]; o != nil {
		return get(o.data, dotted(path))
	}
	return nil
}

func deployments(ns string) string { return "/apis/apps/v1/namespaces/" + ns + "/deployments" }

var resources = map[string]string{ // "<group prefix> <resource>" -> kind
	"/apis/apps/v1 deployments":                  "Deployment",
	"/api/v1 services":                           "Service",
	"/apis/networking.k8s.io/v1 networkpolicies": "NetworkPolicy",
	"/api/v1 pods":                               "Pod",
	"/api/v1 events":                             "Event",
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delay := s.Delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	q := r.URL.Query()
	dryRun := q.Get("dryRun") == "All"
	s.requests = append(s.requests, Request{r.Method, r.URL.Path, dryRun})

	if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
		fail(w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}
	// <prefix>/namespaces/<ns>/<resource>[/<name>]
	i := strings.Index(r.URL.Path, "/namespaces/")
	if i < 0 {
		fail(w, http.StatusNotFound, "NotFound", nil)
		return
	}
	prefix := r.URL.Path[:i]
	parts := strings.Split(r.URL.Path[i+len("/namespaces/"):], "/")
	if len(parts) < 2 || len(parts) > 3 {
		fail(w, http.StatusNotFound, "NotFound", nil)
		return
	}
	ns, resource := parts[0], parts[1]
	kind, known := resources[prefix+" "+resource]
	if !known || !s.namespaces[ns] {
		fail(w, http.StatusNotFound, "NotFound", nil)
		return
	}
	collection := prefix + "/namespaces/" + ns + "/" + resource

	switch {
	case r.Method == http.MethodGet && s.Garbage:
		w.Write([]byte("<html>not the API</html>"))
	case r.Method == http.MethodGet && len(parts) == 2 && kind == "Pod":
		s.list(w, s.pods[ns], q.Get("labelSelector"))
	case r.Method == http.MethodGet && len(parts) == 2 && kind == "Event":
		s.list(w, s.events[ns], "")
	case r.Method == http.MethodGet && len(parts) == 3 && kind != "Pod" && kind != "Event":
		o := s.objects[collection+"/"+parts[2]]
		if o == nil {
			fail(w, http.StatusNotFound, "NotFound", nil)
			return
		}
		reply(w, http.StatusOK, o.view())
	case r.Method == http.MethodPatch && len(parts) == 3 && kind != "Pod" && kind != "Event":
		s.apply(w, r, collection, kind, ns, parts[2], dryRun)
	default:
		fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed", nil)
	}
}

func (s *Server) list(w http.ResponseWriter, items []json.RawMessage, selector string) {
	out := []json.RawMessage{}
	key, value, _ := strings.Cut(selector, "=")
	for _, raw := range items {
		if selector != "" {
			var o struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			if json.Unmarshal(raw, &o) != nil || o.Metadata.Labels[key] != value {
				continue
			}
		}
		out = append(out, raw)
	}
	reply(w, http.StatusOK, map[string]any{"kind": "List", "items": out})
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request, collection, kind, ns, name string, dryRun bool) {
	q := r.URL.Query()
	manager := q.Get("fieldManager")
	if r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
		fail(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType", nil)
		return
	}
	var body map[string]any
	if manager == "" || json.NewDecoder(r.Body).Decode(&body) != nil ||
		body["kind"] != kind || get(body, dotted("metadata.name")) != name || get(body, dotted("metadata.namespace")) != ns {
		fail(w, http.StatusBadRequest, "BadRequest", nil)
		return
	}
	applied := map[string]any{}
	flatten("", body, applied)
	for _, identity := range []string{"apiVersion", "kind", "metadata.name", "metadata.namespace"} {
		delete(applied, dotted(identity))
	}

	key := collection + "/" + name
	o, exists := s.objects[key]
	if !exists {
		o = &object{data: map[string]any{}, owners: map[string]map[string]bool{}, generation: 1}
	}
	var causes []map[string]any
	for path, v := range applied {
		for other := range o.owners[path] {
			if other != manager && !reflect.DeepEqual(get(o.data, path), v) {
				causes = append(causes, map[string]any{
					"reason": "FieldManagerConflict", "field": "." + strings.ReplaceAll(path, sep, "."),
					"message": "conflict with \"" + other + "\"",
				})
			}
		}
	}
	if len(causes) > 0 && q.Get("force") != "true" {
		sort.Slice(causes, func(i, j int) bool { return causes[i]["field"].(string) < causes[j]["field"].(string) })
		fail(w, http.StatusConflict, "Conflict", causes)
		return
	}
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	if dryRun {
		reply(w, status, body)
		return
	}
	before, _ := json.Marshal(o.data["spec"])
	for path, owners := range o.owners { // fields this manager no longer sets
		if _, still := applied[path]; owners[manager] && !still {
			delete(owners, manager)
			if len(owners) == 0 {
				remove(o.data, path)
				delete(o.owners, path)
			}
		}
	}
	for path, v := range applied {
		if !reflect.DeepEqual(get(o.data, path), v) || o.owners[path] == nil {
			o.owners[path] = map[string]bool{}
		}
		o.owners[path][manager] = true
		set(o.data, path, v)
	}
	for _, identity := range []string{"apiVersion", "kind", "metadata.name", "metadata.namespace"} {
		set(o.data, dotted(identity), get(body, dotted(identity)))
	}
	if after, _ := json.Marshal(o.data["spec"]); exists && string(before) != string(after) {
		o.generation++
	}
	s.objects[key] = o
	reply(w, status, o.view())
}

func (o *object) view() map[string]any {
	raw, _ := json.Marshal(o.data)
	var v map[string]any
	json.Unmarshal(raw, &v)
	set(v, dotted("metadata.generation"), o.generation)
	if o.status != nil {
		v["status"] = o.status
	}
	return v
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, reason string, causes []map[string]any) {
	st := map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "code": status}
	if causes != nil {
		st["details"] = map[string]any{"causes": causes}
	}
	reply(w, status, st)
}

// Field paths are kept as keys joined by sep, because label keys contain
// dots. dotted turns a simple "a.b.c" path, as tests write it, into one.
const sep = "\x00"

func dotted(path string) string { return strings.ReplaceAll(path, ".", sep) }

// flatten lists every leaf of v by path. Lists and empty maps are leaves.
func flatten(prefix string, v any, out map[string]any) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		out[prefix] = v
		return
	}
	for k, child := range m {
		p := k
		if prefix != "" {
			p = prefix + sep + k
		}
		flatten(p, child, out)
	}
}

func get(m map[string]any, path string) any {
	key, rest, more := strings.Cut(path, sep)
	v, ok := m[key]
	if !ok || !more {
		return v
	}
	child, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return get(child, rest)
}

func set(m map[string]any, path string, value any) {
	key, rest, more := strings.Cut(path, sep)
	if !more {
		m[key] = value
		return
	}
	child, ok := m[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		m[key] = child
	}
	set(child, rest, value)
}

func remove(m map[string]any, path string) {
	key, rest, more := strings.Cut(path, sep)
	if !more {
		delete(m, key)
		return
	}
	if child, ok := m[key].(map[string]any); ok {
		remove(child, rest)
		if len(child) == 0 {
			delete(m, key)
		}
	}
}
