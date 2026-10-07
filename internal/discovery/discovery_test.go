package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
)

const watch = "homerun2.sthings.io/watch-expiry"

func secret(ns, name string, labelled bool, ann map[string]string, keys ...string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: ann}, Data: map[string][]byte{}}
	if labelled {
		s.Labels = map[string]string{watch: "true"}
	}
	for _, k := range keys {
		s.Data[k] = []byte("x")
	}
	return s
}

func vaultSecret(ns, name string, ann map[string]string, keys ...string) *corev1.Secret {
	s := secret(ns, name, true, ann, keys...)
	s.Labels[watch] = "vault-token"
	return s
}

func testProfile(t *testing.T) *profile.SchedulePitcherProfile {
	t.Helper()
	p, err := profile.Parse([]byte(`
apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
metadata: {name: machinery}
spec:
  defaults: {assignee: patrick.hermann, tags: [expiry]}
  discovery: {enabled: true}
  checks:
    - {id: flux-system.github-auth, type: github-token-expiry, tokenFrom: {env: X}, description: own}
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscover(t *testing.T) {
	client := fake.NewClientset(
		secret("flux-system", "git-token-auth", true, nil, "username", "password"),
		secret("tekton-ci", "github-runner-token", true, map[string]string{
			AnnotationURL: "https://github.com/settings/personal-access-tokens", AnnotationTags: "pat, runner",
		}, "GITHUB_TOKEN"),
		secret("argocd", "repo-creds", true, map[string]string{AnnotationTokenKey: "pw", AnnotationCheckID: "argocd-repo"}, "pw", "url"),
		secret("backstage", "unlabelled", false, nil, "GITHUB_TOKEN"),
		secret("dapr", "ambiguous", true, nil, "a", "b"),
		secret("kargo", "tls", true, map[string]string{AnnotationCheckType: "tls-endpoint"}, "tls.crt"),
		vaultSecret("openbao", "openbao-transit-seal", map[string]string{
			AnnotationVaultAddr: "https://vault-vsphere.example:8200", AnnotationCAFile: "/etc/ssl/custom/trust-bundle.pem",
		}, "token", "addr"),
		vaultSecret("cert-manager", "no-addr", nil, "token"),
	)
	p := testProfile(t)
	d := &Discoverer{Client: client, Config: p.Spec.Discovery, Profile: p}
	found, complete, err := d.Discover(context.Background())
	if !complete {
		t.Fatal("incomplete")
	}
	if err == nil || !strings.Contains(err.Error(), "dapr/ambiguous") || !strings.Contains(err.Error(), "cannot be discovered") {
		t.Errorf("err = %v", err)
	}
	ids := map[string]profile.Check{}
	for _, c := range found {
		ids[c.ID] = c
	}
	if len(ids) != 4 {
		t.Fatalf("found = %v", ids)
	}
	seal := ids["openbao.openbao-transit-seal"]
	if seal.Type != profile.TypeVaultTokenTTL || seal.Addr != "https://vault-vsphere.example:8200" ||
		seal.TokenFrom.SecretKeyRef.Key != "token" || seal.CAFile == "" || seal.Thresholds.Error.D().Hours() != 7*24 {
		t.Errorf("seal = %+v", seal)
	}
	if !strings.Contains(err.Error(), "cert-manager/no-addr") || !strings.Contains(err.Error(), AnnotationVaultAddr) {
		t.Errorf("missing vault-addr not reported: %v", err)
	}
	flux := ids["flux-system.git-token-auth"]
	if ref := flux.TokenFrom.SecretKeyRef; ref.Key != "password" || ref.Namespace != "flux-system" {
		t.Errorf("flux key = %+v", ref)
	}
	if flux.Origin != profile.OriginDiscovered || flux.Assignee != "patrick.hermann" || flux.Thresholds.Error.D().Hours() != 14*24 || flux.Schedule == "" {
		t.Errorf("defaults not applied: %+v", flux)
	}
	runner := ids["tekton-ci.github-runner-token"]
	if runner.TokenFrom.SecretKeyRef.Key != "GITHUB_TOKEN" || strings.Join(runner.Tags, ",") != "expiry,pat,runner" || runner.URL == "" {
		t.Errorf("runner = %+v", runner)
	}
	if ids["argocd-repo"].TokenFrom.SecretKeyRef.Key != "pw" {
		t.Errorf("argocd = %+v", ids["argocd-repo"])
	}
}

func TestDiscoverNamespacesAndListErrors(t *testing.T) {
	client := fake.NewClientset(
		secret("a", "t", true, nil, "token"),
		secret("b", "t", true, nil, "token"),
	)
	client.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "b" {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	p := testProfile(t)
	cfg := p.Spec.Discovery
	cfg.Namespaces = []string{"a", "b"}
	d := &Discoverer{Client: client, Config: cfg, Profile: p}
	found, complete, err := d.Discover(context.Background())
	if complete || err == nil || len(found) != 1 || found[0].ID != "a.t" {
		t.Fatalf("found %v complete %v err %v", found, complete, err)
	}
}

type fakeSyncer struct{ got []profile.Check }

func (f *fakeSyncer) Sync(cs []profile.Check) error { f.got = cs; return nil }

func ids(cs []profile.Check) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return strings.Join(out, ",")
}

func TestOnceMergesAndKeepsOnIncompleteScan(t *testing.T) {
	p := testProfile(t)
	client := fake.NewClientset(
		secret("flux-system", "github-auth", true, nil, "password"), // shadowed by the profile check
		secret("tekton-ci", "runner", true, nil, "token"),
	)
	d := &Discoverer{Client: client, Config: p.Spec.Discovery, Profile: p}
	s := &fakeSyncer{}
	d.Once(context.Background(), s, func() []profile.Check { return nil })
	if ids(s.got) != "flux-system.github-auth,tekton-ci.runner" || s.got[0].Description != "own" {
		t.Fatalf("synced %s (%+v)", ids(s.got), s.got)
	}

	// The API fails: the known discovered checks stay.
	current := s.got
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver down")
	})
	d.Once(context.Background(), s, func() []profile.Check { return current })
	if ids(s.got) != "flux-system.github-auth,tekton-ci.runner" {
		t.Fatalf("after failed scan: %s", ids(s.got))
	}
}
