// Package kube is a small Kubernetes API client on net/http. It does three
// things: server-side apply of an object, read one Deployment, and list the
// pods and events of a namespace.
package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

// maxReply bounds how much of any reply is read, so one large list cannot
// take Shipkeeper past its memory budget.
const maxReply = 8 << 20

// Client talks to one Kubernetes API server.
type Client struct {
	BaseURL      string       // e.g. https://kubernetes.default.svc
	Token        string       // bearer token, used when TokenFile is empty
	TokenFile    string       // ServiceAccount token path, read fresh per call
	FieldManager string       // the name fields are owned under
	HTTP         *http.Client // must carry a timeout
}

var collections = map[string]string{
	"apps/v1 Deployment":                 "/apis/apps/v1/namespaces/%/deployments",
	"v1 Service":                         "/api/v1/namespaces/%/services",
	"networking.k8s.io/v1 NetworkPolicy": "/apis/networking.k8s.io/v1/namespaces/%/networkpolicies",
}

func collection(apiVersion, kind, namespace string) (string, bool) {
	c, ok := collections[apiVersion+" "+kind]
	if !ok {
		return "", false
	}
	return string(bytes.Replace([]byte(c), []byte("%"), []byte(url.PathEscape(namespace)), 1)), true
}

// Apply sends obj with server-side apply. It never forces: when another
// field manager owns a field with a different value the API answers with a
// conflict, nothing is changed, and Apply returns errs.ApplyConflict with
// the field paths. With dryRun the server checks the request, conflicts
// included, and stores nothing.
func (c *Client) Apply(ctx context.Context, obj Object, dryRun bool) error {
	path, ok := collection(obj.APIVersion, obj.Kind, obj.Namespace)
	if !ok {
		return errs.KubeKind
	}
	q := url.Values{"fieldManager": {c.FieldManager}, "force": {"false"}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	status, body, err := c.do(ctx, http.MethodPatch, path+"/"+url.PathEscape(obj.Name)+"?"+q.Encode(), obj.Body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusConflict:
		return errs.ApplyConflict.WithFields(conflictFields(body))
	}
	return statusError(status)
}

var fieldPathRE = regexp.MustCompile(`^[A-Za-z0-9_.\[\]="/-]{1,200}$`)

// conflictFields pulls the conflicting field paths out of a Status reply.
// Only strings shaped like a field path are kept.
func conflictFields(body []byte) []string {
	var st struct {
		Details struct {
			Causes []struct {
				Field string `json:"field"`
			} `json:"causes"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &st) != nil {
		return nil
	}
	var out []string
	for _, c := range st.Details.Causes {
		if fieldPathRE.MatchString(c.Field) {
			out = append(out, c.Field)
		}
	}
	sort.Strings(out)
	return out
}

// Observe reads the Deployment called name, the pods carrying
// labelKey=name, and every event in the namespace.
func (c *Client) Observe(ctx context.Context, namespace, name, labelKey string) (Observed, error) {
	var o Observed
	ns := "/namespaces/" + url.PathEscape(namespace)
	if err := c.get(ctx, "/apis/apps/v1"+ns+"/deployments/"+url.PathEscape(name), &o.Deployment); err != nil {
		return o, err
	}
	pods, events, err := c.listPodsAndEvents(ctx, ns, name, labelKey)
	if err != nil {
		return o, err
	}
	o.Pods, o.Events = pods, events
	return o, nil
}

// ObserveExisting inspects an existing workload in namespace when Plainforge
// has no app.yaml for it yet (such as a StatefulSet like Logbook or a service
// deployed directly from manifests). When neither a Deployment nor matching
// pods exist yet, hasDeployment is false and o.Pods is empty.
func (c *Client) ObserveExisting(ctx context.Context, namespace, name, labelKey string) (Observed, bool, error) {
	var o Observed
	ns := "/namespaces/" + url.PathEscape(namespace)
	status, body, err := c.do(ctx, http.MethodGet, "/apis/apps/v1"+ns+"/deployments/"+url.PathEscape(name), nil)
	if err != nil {
		return o, false, err
	}
	hasDeployment := false
	switch status {
	case http.StatusOK:
		if json.Unmarshal(body, &o.Deployment) != nil {
			return o, false, errs.KubeBadReply
		}
		hasDeployment = true
	case http.StatusNotFound:
		hasDeployment = false
	default:
		return o, false, statusError(status)
	}
	pods, events, err := c.listPodsAndEvents(ctx, ns, name, labelKey)
	if err != nil {
		return o, false, err
	}
	o.Pods, o.Events = pods, events
	return o, hasDeployment, nil
}

func (c *Client) listPodsAndEvents(ctx context.Context, ns, name, labelKey string) ([]Pod, []Event, error) {
	var pods struct {
		Items []Pod `json:"items"`
	}
	sel := url.Values{"labelSelector": {labelKey + "=" + name}}
	if err := c.get(ctx, "/api/v1"+ns+"/pods?"+sel.Encode(), &pods); err != nil {
		return nil, nil, err
	}
	var events struct {
		Items []Event `json:"items"`
	}
	if err := c.get(ctx, "/api/v1"+ns+"/events", &events); err != nil {
		return nil, nil, err
	}
	return pods.Items, events.Items, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	status, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(status)
	}
	if json.Unmarshal(body, out) != nil {
		return errs.KubeBadReply
	}
	return nil
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errs.KubeRefused
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return errs.KubeTimeout
	}
	return errs.KubeUnexpected
}

// do sends one request. The returned errors come from the catalogue only;
// the transport error, which can hold a host name or an address, is dropped.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	token, err := c.bearerToken()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errs.Config
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		// JSON is YAML, and this is the media type server-side apply asks for.
		req.Header.Set("Content-Type", "application/apply-patch+yaml")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, transportError(err)
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return 0, nil, transportError(err)
	}
	if len(reply) > maxReply {
		return 0, nil, errs.KubeUnexpected
	}
	return resp.StatusCode, reply, nil
}

func (c *Client) bearerToken() (string, error) {
	if c.TokenFile == "" {
		return c.Token, nil
	}
	raw, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", errs.Config
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errs.Config
	}
	return tok, nil
}

func transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return errs.KubeTimeout
	}
	return errs.KubeUnreachable
}
