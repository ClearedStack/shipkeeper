// Package sanitize removes cluster details from text before it leaves
// Shipkeeper: node names, pod host names, internal addresses and internal
// cluster DNS names.
//
// The rules, in the order they run:
//
//  1. Known names. Every node name, pod name, pod host name and address
//     seen in the objects being reported is replaced wherever it appears.
//  2. Internal DNS names. A dotted name is internal when it ends in the
//     cluster domain, has a "svc" or "pod" label after its first label, or
//     is exactly "<name>.<managed namespace>".
//  3. IPv6 addresses.
//  4. IPv4 addresses, with or without a /prefix.
//
// When unsure the rules replace too much rather than too little.
package sanitize

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// Placeholders written in place of what was removed.
const (
	Node     = "[node]"
	Pod      = "[pod]"
	Address  = "[address]"
	Internal = "[internal-name]"
)

// Config says what is known about the cluster.
type Config struct {
	ClusterDomain string            // e.g. cluster.local
	Namespaces    []string          // managed namespaces
	Nodes         []string          // node names seen
	Pods          map[string]string // pod name or host name -> what to write instead
	Addresses     []string          // addresses seen
}

// Scrubber applies the rules for one Config.
type Scrubber struct {
	known      *regexp.Regexp
	replace    map[string]string // lower-case known value -> placeholder
	domain     []string
	namespaces map[string]bool
}

var (
	dottedRE = regexp.MustCompile(`[A-Za-z0-9_\[\]-]+(\.[A-Za-z0-9_\[\]-]+)+`)
	ipv6RE   = regexp.MustCompile(`[0-9A-Fa-f:.]{2,}(/[0-9]{1,3})?`)
	ipv4RE   = regexp.MustCompile(`([0-9]{1,3}\.){3}[0-9]{1,3}(/[0-9]{1,2})?`)
)

// New builds a Scrubber.
func New(cfg Config) *Scrubber {
	s := &Scrubber{replace: map[string]string{}, namespaces: map[string]bool{}}
	for _, l := range strings.Split(strings.ToLower(cfg.ClusterDomain), ".") {
		if l != "" {
			s.domain = append(s.domain, l)
		}
	}
	for _, ns := range cfg.Namespaces {
		s.namespaces[strings.ToLower(ns)] = true
	}
	// Later groups win, so a value that is both a pod name and a node name
	// is treated as a node name.
	for _, a := range cfg.Addresses {
		s.add(a, Address)
	}
	for name, alias := range cfg.Pods {
		s.add(name, alias)
	}
	for _, n := range cfg.Nodes {
		s.add(n, Node)
	}
	if len(s.replace) > 0 {
		keys := make([]string, 0, len(s.replace))
		for k := range s.replace {
			keys = append(keys, k)
		}
		// Longest first, so a name is never cut by a shorter one inside it.
		sort.Slice(keys, func(i, j int) bool {
			if len(keys[i]) != len(keys[j]) {
				return len(keys[i]) > len(keys[j])
			}
			return keys[i] < keys[j]
		})
		for i, k := range keys {
			keys[i] = regexp.QuoteMeta(k)
		}
		s.known = regexp.MustCompile("(?i)" + strings.Join(keys, "|"))
	}
	return s
}

func (s *Scrubber) add(value, placeholder string) {
	if value != "" {
		s.replace[strings.ToLower(value)] = placeholder
	}
}

// Text returns in with every cluster detail replaced by a placeholder.
func (s *Scrubber) Text(in string) string {
	out := in
	if s.known != nil {
		out = s.known.ReplaceAllStringFunc(out, func(m string) string { return s.replace[strings.ToLower(m)] })
	}
	out = dottedRE.ReplaceAllStringFunc(out, s.dotted)
	out = ipv6RE.ReplaceAllStringFunc(out, ipv6)
	out = ipv4RE.ReplaceAllString(out, Address)
	return out
}

// Leaks reports whether out still holds something the rules remove. It is
// the last check before a report is sent.
func (s *Scrubber) Leaks(out string) bool {
	return s.Text(out) != out
}

func (s *Scrubber) dotted(name string) string {
	labels := strings.Split(strings.ToLower(name), ".")
	if len(labels) == 2 && s.namespaces[labels[1]] {
		return Internal
	}
	for _, l := range labels[1:] {
		if l == "svc" || l == "pod" {
			return Internal
		}
	}
	if d := len(s.domain); d > 0 && len(labels) > d {
		tail := labels[len(labels)-d:]
		same := true
		for i := range tail {
			same = same && tail[i] == s.domain[i]
		}
		if same {
			return Internal
		}
	}
	return name
}

func ipv6(candidate string) string {
	if strings.Count(candidate, ":") < 2 {
		return candidate
	}
	body, prefix, hasPrefix := strings.Cut(candidate, "/")
	head := ""
	if strings.HasPrefix(body, ":") && !strings.HasPrefix(body, "::") {
		head, body = ":", body[1:] // "label:fd00::1"
	}
	// Try the whole run, then peel sentence punctuation off its end.
	for end := len(body); end > 0; end-- {
		if addr, err := netip.ParseAddr(body[:end]); err == nil && addr.Is6() {
			rest := body[end:]
			if hasPrefix && rest != "" {
				rest += "/" + prefix
			}
			return head + Address + rest
		}
		if last := body[end-1]; last != '.' && last != ':' {
			break
		}
	}
	return candidate
}
