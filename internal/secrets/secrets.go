// Package secrets resolves profile ValueFrom references: a Kubernetes Secret
// key, an environment variable or a file.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
)

// SecretGetter reads one key of a Kubernetes Secret.
type SecretGetter interface {
	GetSecretKey(ctx context.Context, namespace, name, key string) (string, error)
}

// Resolver resolves ValueFrom references. The Kubernetes client is created on
// first use, so profiles without secretKeyRef never need cluster access.
type Resolver struct {
	// DefaultNamespace is used for secretKeyRefs without a namespace.
	DefaultNamespace string

	mu     sync.Mutex
	getter SecretGetter
}

// NewResolver returns a resolver. getter may be nil; a Kubernetes client is
// then built from the in-cluster config or KUBECONFIG when first needed.
func NewResolver(getter SecretGetter) *Resolver {
	return &Resolver{DefaultNamespace: podNamespace(), getter: getter}
}

// Resolve returns the referenced value with surrounding whitespace trimmed.
func (r *Resolver) Resolve(ctx context.Context, v *profile.ValueFrom) (string, error) {
	if v == nil {
		return "", errors.New("no value reference")
	}
	switch {
	case v.Env != "":
		val, ok := os.LookupEnv(v.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", v.Env)
		}
		return strings.TrimSpace(val), nil
	case v.File != "":
		data, err := os.ReadFile(v.File)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", v.File, err)
		}
		return strings.TrimSpace(string(data)), nil
	case v.SecretKeyRef != nil:
		ref := v.SecretKeyRef
		ns := ref.Namespace
		if ns == "" {
			ns = r.DefaultNamespace
		}
		g, err := r.secretGetter()
		if err != nil {
			return "", err
		}
		val, err := g.GetSecretKey(ctx, ns, ref.Name, ref.Key)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(val), nil
	}
	return "", errors.New("empty value reference")
}

func (r *Resolver) secretGetter() (SecretGetter, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getter != nil {
		return r.getter, nil
	}
	g, err := newKubeGetter()
	if err != nil {
		return nil, err
	}
	r.getter = g
	return g, nil
}

type kubeGetter struct {
	client kubernetes.Interface
}

func newKubeGetter() (*kubeGetter, error) {
	client, err := NewKubeClient()
	if err != nil {
		return nil, err
	}
	return &kubeGetter{client: client}, nil
}

// NewKubeClient builds a client from the in-cluster config or KUBECONFIG.
func NewKubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no kubernetes config (in-cluster or KUBECONFIG): %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return client, nil
}

func (k *kubeGetter) GetSecretKey(ctx context.Context, namespace, name, key string) (string, error) {
	s, err := k.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading secret %s/%s: %w", namespace, name, err)
	}
	val, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, name, key)
	}
	return string(val), nil
}

// podNamespace returns the namespace the pod runs in, or "default".
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return "default"
}
