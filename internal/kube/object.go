package kube

// Object is one manifest ready to be applied: who it is, and its JSON body.
type Object struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	Body       []byte
}
