// Package state persists the controller's long-lived state — the Gitea runner
// identities of its workers — in a Kubernetes Secret, so a restart keeps the
// same runner identity instead of registering a new one each time.
package state

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Identity is a persisted Gitea runner registration.
type Identity struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

const dataKey = "identities"

// Store keeps a map of worker key -> identity in one Secret.
type Store struct {
	cs   kubernetes.Interface
	ns   string
	name string
}

// New returns a store backed by the named Secret in namespace.
func New(cs kubernetes.Interface, namespace, name string) *Store {
	return &Store{cs: cs, ns: namespace, name: name}
}

func (s *Store) load(ctx context.Context) (map[string]Identity, error) {
	sec, err := s.cs.CoreV1().Secrets(s.ns).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]Identity{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Identity{}
	if b := sec.Data[dataKey]; len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Get returns the identity stored under key, if any.
func (s *Store) Get(ctx context.Context, key string) (Identity, bool, error) {
	m, err := s.load(ctx)
	if err != nil {
		return Identity{}, false, err
	}
	id, ok := m[key]
	return id, ok, nil
}

// UUIDs returns the uuid of every identity this controller has registered, so
// the runner audit can tell our own runners from foreign ones.
func (s *Store) UUIDs(ctx context.Context) (map[string]bool, error) {
	m, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(m))
	for _, id := range m {
		if id.UUID != "" {
			out[id.UUID] = true
		}
	}
	return out, nil
}

// Put stores the identity under key.
func (s *Store) Put(ctx context.Context, key string, id Identity) error {
	m, err := s.load(ctx)
	if err != nil {
		return err
	}
	m[key] = id
	return s.write(ctx, m)
}

// EnsureWritable proves the state Secret can be persisted before anything is
// registered against Gitea. Registration creates a runner record that can only
// be removed through the Gitea API, so registering first and failing to persist
// afterwards would leave a zombie runner behind on every restart.
func (s *Store) EnsureWritable(ctx context.Context) error {
	m, err := s.load(ctx)
	if err != nil {
		return err
	}
	return s.write(ctx, m)
}

func (s *Store) write(ctx context.Context, m map[string]Identity) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{dataKey: b},
	}
	if _, err := s.cs.CoreV1().Secrets(s.ns).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		if _, err := s.cs.CoreV1().Secrets(s.ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	return nil
}
