// Package scope captures the two deployment shapes of the controller: a
// namespace-scoped instance (deployable with only that namespace's kubeconfig)
// and a cluster-scoped one deployed by an administrator. Everything that
// differs between the two is confined here.
package scope

import "fmt"

// Kind is the scope kind.
type Kind string

const (
	// Namespace scope only touches the controller's own namespace.
	Namespace Kind = "namespace"
	// Cluster scope may act across namespaces.
	Cluster Kind = "cluster"
)

// Scope is a validated deployment scope.
type Scope struct {
	kind            Kind
	targetNamespace string
}

// New validates the scope kind and target namespace. v1 always creates executor
// pods in targetNamespace, even in cluster scope.
func New(kind, targetNamespace string) (Scope, error) {
	switch Kind(kind) {
	case Namespace:
		if targetNamespace == "" {
			return Scope{}, fmt.Errorf("namespace scope requires TARGET_NAMESPACE")
		}
		return Scope{kind: Namespace, targetNamespace: targetNamespace}, nil
	case Cluster:
		if targetNamespace == "" {
			return Scope{}, fmt.Errorf("cluster scope requires TARGET_NAMESPACE (v1: executor pods land here)")
		}
		return Scope{kind: Cluster, targetNamespace: targetNamespace}, nil
	default:
		return Scope{}, fmt.Errorf("unknown scope %q", kind)
	}
}

// Kind returns the scope kind.
func (s Scope) Kind() Kind { return s.kind }

// ExecutorNamespace is where executor pods and capability Secrets are created.
func (s Scope) ExecutorNamespace() string { return s.targetNamespace }

// ClusterWide reports whether the controller may list resources cluster-wide.
func (s Scope) ClusterWide() bool { return s.kind == Cluster }
