package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
)

type fakeGetter map[string]string

func (f fakeGetter) GetSecretKey(_ context.Context, ns, name, key string) (string, error) {
	v, ok := f[ns+"/"+name+"/"+key]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return v, nil
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	r := NewResolver(fakeGetter{"tekton-ci/runner/GITHUB_TOKEN": "ghp_a\n", "home/x/k": "own-ns"})
	r.DefaultNamespace = "home"

	t.Setenv("SP_TEST_TOKEN", " ghp_env ")
	file := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(file, []byte("ghp_file\n"), 0o600)

	tests := []struct {
		ref  profile.ValueFrom
		want string
	}{
		{profile.ValueFrom{SecretKeyRef: &profile.SecretKeyRef{Namespace: "tekton-ci", Name: "runner", Key: "GITHUB_TOKEN"}}, "ghp_a"},
		{profile.ValueFrom{SecretKeyRef: &profile.SecretKeyRef{Name: "x", Key: "k"}}, "own-ns"},
		{profile.ValueFrom{Env: "SP_TEST_TOKEN"}, "ghp_env"},
		{profile.ValueFrom{File: file}, "ghp_file"},
	}
	for _, tt := range tests {
		got, err := r.Resolve(ctx, &tt.ref)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%+v) = %q, %v; want %q", tt.ref, got, err, tt.want)
		}
	}

	for _, bad := range []profile.ValueFrom{{Env: "SP_TEST_UNSET"}, {File: "/does/not/exist"}, {SecretKeyRef: &profile.SecretKeyRef{Name: "nope", Key: "k"}}} {
		if _, err := r.Resolve(ctx, &bad); err == nil {
			t.Errorf("Resolve(%+v) succeeded", bad)
		}
	}
}
