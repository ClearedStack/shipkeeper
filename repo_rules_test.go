package shipkeeper_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNoEmDashesAnywhere(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(b) {
			return nil
		}
		if strings.ContainsRune(string(b), '\u2014') {
			t.Errorf("%s contains an em dash (U+2014)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStandardLibraryOnlyAndModulePath(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "module github.com/ClearedStack/shipkeeper\n") {
		t.Fatalf("unexpected module path in go.mod:\n%s", text)
	}
	if strings.Contains(text, "require") {
		t.Fatalf("external dependency found in go.mod:\n%s", text)
	}
	for _, forbidden := range []string{"Dockerfile", "Containerfile", "go.sum"} {
		if _, err := os.Stat(forbidden); err == nil {
			t.Errorf("%s must not exist", forbidden)
		}
	}

	out, err := exec.Command("go", "list", "-f", `{{range .Imports}}{{.}}{{"\n"}}{{end}}{{range .TestImports}}{{.}}{{"\n"}}{{end}}`, "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list imports: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" || strings.HasPrefix(pkg, "github.com/ClearedStack/shipkeeper") {
			continue
		}
		first, _, _ := strings.Cut(pkg, "/")
		if strings.Contains(first, ".") {
			t.Errorf("third-party import %q is not allowed (standard library only)", pkg)
		}
	}
}

func TestDeployManifestsAndKoConfig(t *testing.T) {
	digestPinned := regexp.MustCompile(`@sha256:[0-9a-f]{64}`)

	ko, err := os.ReadFile(".ko.yaml")
	if err != nil {
		t.Fatal(err)
	}
	koText := string(ko)
	if !digestPinned.MatchString(koText) || !strings.Contains(koText, "defaultBaseImage:") {
		t.Fatalf(".ko.yaml base image is not pinned by sha256 digest:\n%s", koText)
	}
	if !strings.Contains(koText, "CGO_ENABLED=0") || !strings.Contains(koText, "linux/arm64") {
		t.Fatalf(".ko.yaml missing CGO_ENABLED=0 or linux/arm64:\n%s", koText)
	}

	sk, err := os.ReadFile("deploy/shipkeeper.yaml")
	if err != nil {
		t.Fatal(err)
	}
	skText := string(sk)
	if !digestPinned.MatchString(skText) {
		t.Fatalf("deploy/shipkeeper.yaml image is not pinned by sha256 digest")
	}
	requiredInShipkeeper := []string{
		"namespace: clearedstack",
		"serviceAccountName: shipkeeper",
		"automountServiceAccountToken: true",
		"image: lazarette.clearedstack.svc.cluster.local:5000/shipkeeper@sha256:",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"key: kubernetes.io/arch",
		"operator: In",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		"operator: NotIn",
		`values: ["cm3588-plus"]`,
		"memory: 48Mi",
		"path: /healthz",
	}
	for _, want := range requiredInShipkeeper {
		if !strings.Contains(skText, want) {
			t.Errorf("deploy/shipkeeper.yaml is missing %q", want)
		}
	}

	rbac, err := os.ReadFile("deploy/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rbacText := string(rbac)
	for _, want := range []string{
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"namespace: clearedstack",
		"namespace: cs-workers",
	} {
		if !strings.Contains(rbacText, want) {
			t.Errorf("deploy/rbac.yaml is missing %q", want)
		}
	}
	rbacBody := stripYAMLComments(rbacText)
	for _, forbidden := range []string{"ClusterRole", "secrets", "delete"} {
		if strings.Contains(rbacBody, forbidden) {
			t.Errorf("deploy/rbac.yaml must not grant %q", forbidden)
		}
	}

	np, err := os.ReadFile("deploy/networkpolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	npText := string(np)
	for _, want := range []string{
		"name: default-deny",
		"name: shipkeeper",
		"namespace: clearedstack",
		"- Ingress",
		"- Egress",
		"ingress: []",
		"app.kubernetes.io/name: plainforge",
		"app.kubernetes.io/name: lazarette",
		"app.kubernetes.io/name: muster",
		"kubernetes.io/metadata.name: kube-system",
		"port: 6443",
	} {
		if !strings.Contains(npText, want) {
			t.Errorf("deploy/networkpolicy.yaml is missing %q", want)
		}
	}
	if strings.Contains(npText, "0.0.0.0/0") {
		t.Error("deploy/networkpolicy.yaml must not allow public internet egress (0.0.0.0/0)")
	}

	ns, err := os.ReadFile("deploy/namespace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	nsText := string(ns)
	for _, want := range []string{"name: clearedstack", "name: cs-workers"} {
		if !strings.Contains(nsText, want) {
			t.Errorf("deploy/namespace.yaml is missing %q", want)
		}
	}

	// Ensure no manifest in deploy/ embeds secrets or keys.
	entries, err := os.ReadDir("deploy")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("deploy", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		txt := string(raw)
		for _, bad := range []string{"kind: Secret", "PRIVATE KEY", "password:"} {
			if strings.Contains(txt, bad) {
				t.Errorf("deploy/%s contains forbidden token %q", e.Name(), bad)
			}
		}
	}
}

func stripYAMLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
