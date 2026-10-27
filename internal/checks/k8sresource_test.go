package checks

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

var (
	volumeGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}
	vmGVR     = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
)

// volume is a Longhorn volume in longhorn-system bound to pvc.
func volume(name, robustness, pvc string) *unstructured.Unstructured {
	st := map[string]any{"state": "attached"}
	if robustness != "" {
		st["robustness"] = robustness
	}
	if pvc != "" {
		st["kubernetesStatus"] = map[string]any{"pvcName": pvc, "namespace": "default"}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2",
		"kind":       "Volume",
		"metadata": map[string]any{
			"name": name, "namespace": "longhorn-system",
			"labels": map[string]any{"longhornvolume": name},
		},
		"spec":   map[string]any{"numberOfReplicas": int64(3)},
		"status": st,
	}}
}

func vm(name string, conds ...map[string]any) *unstructured.Unstructured {
	var cs []any
	for _, c := range conds {
		cs = append(cs, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   map[string]any{"name": name, "namespace": "vms"},
		"status":     map[string]any{"conditions": cs},
	}}
}

func strp(s string) *string { return &s }

// longhornRules is the first use case of the check (harvester#573).
var longhornRules = []profile.Rule{
	{Path: ".status.robustness", NotIn: []string{"healthy", "unknown"}, Band: "error"},
	{Path: ".status.robustness", In: []string{"faulted"}, Band: "critical"},
}

func newFakeDynamic(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		volumeGVR: "VolumeList",
		vmGVR:     "VirtualMachineList",
	}, objs...)
}

func runK8sResource(t *testing.T, k *K8sResource, prior Prior, objs ...runtime.Object) Result {
	t.Helper()
	kube := &Kube{Client: newFakeDynamic(objs...)}
	k.Kube = func() (*Kube, error) { return kube, nil }
	if k.Now == nil {
		k.Now = func() time.Time { return srNow }
	}
	res, err := k.RunWithPrior(context.Background(), prior)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func longhorn() *K8sResource {
	return &K8sResource{
		GVR:         volumeGVR,
		Namespaces:  []string{"longhorn-system"},
		Rules:       longhornRules,
		DisplayPath: ".status.kubernetesStatus.pvcName",
	}
}

func TestK8sResourceLonghorn(t *testing.T) {
	tests := []struct {
		name    string
		objs    []runtime.Object
		band    status.Band
		summary string
		problem string
	}{
		{
			name: "all healthy or detached",
			objs: []runtime.Object{
				volume("pvc-1", "healthy", "data-a"),
				volume("pvc-2", "unknown", "data-b"),
			},
			band: status.OK, summary: "2 volumes: 2 ok",
		},
		{
			name: "one degraded",
			objs: []runtime.Object{
				volume("pvc-1", "healthy", "data-a"),
				volume("pvc-2", "degraded", "data-b"),
			},
			band:    status.Error,
			summary: "2 volumes: 1 ok, 1 error",
			problem: "data-b error: .status.robustness=degraded",
		},
		{
			name: "faulted is critical, worst first",
			objs: []runtime.Object{
				volume("pvc-1", "degraded", "data-a"),
				volume("pvc-2", "faulted", "data-b"),
				volume("pvc-3", "healthy", "data-c"),
			},
			band:    status.Critical,
			summary: "3 volumes: 1 ok, 1 error, 1 critical",
			problem: "data-b critical: .status.robustness=faulted; data-a error: .status.robustness=degraded",
		},
		{
			name:    "no pvc: namespace/name",
			objs:    []runtime.Object{volume("pvc-9", "degraded", "")},
			band:    status.Error,
			summary: "1 volume: 1 error",
			problem: "longhorn-system/pvc-9 error: .status.robustness=degraded",
		},
		{
			name:    "missing field matches notIn",
			objs:    []runtime.Object{volume("pvc-9", "", "data-a")},
			band:    status.Error,
			summary: "1 volume: 1 error",
			problem: "data-a error: .status.robustness missing",
		},
		{
			name:    "nothing found",
			band:    status.Warning,
			summary: "no volumes.longhorn.io found",
			problem: "no volumes.longhorn.io matches the selector",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runK8sResource(t, longhorn(), Prior{}, tt.objs...)
			if res.MinBand != tt.band {
				t.Errorf("band = %s, want %s", res.MinBand, tt.band)
			}
			if res.Summary != tt.summary {
				t.Errorf("summary = %q, want %q", res.Summary, tt.summary)
			}
			if res.Problem != tt.problem {
				t.Errorf("problem = %q, want %q", res.Problem, tt.problem)
			}
			if res.BadSince != nil {
				t.Errorf("BadSince without minAge: %v", res.BadSince)
			}
		})
	}
}

func TestK8sResourceOperators(t *testing.T) {
	obj := volume("pvc-1", "degraded", "data-a")
	tests := []struct {
		name  string
		rule  profile.Rule
		match bool
	}{
		{"equals", profile.Rule{Path: ".status.robustness", Equals: strp("degraded")}, true},
		{"equals other", profile.Rule{Path: ".status.robustness", Equals: strp("healthy")}, false},
		{"equals number", profile.Rule{Path: ".spec.numberOfReplicas", Equals: strp("3")}, true},
		{"notEquals", profile.Rule{Path: ".status.robustness", NotEquals: strp("healthy")}, true},
		{"notEquals same", profile.Rule{Path: ".status.robustness", NotEquals: strp("degraded")}, false},
		{"notEquals missing", profile.Rule{Path: ".status.nope", NotEquals: strp("x")}, true},
		{"in", profile.Rule{Path: ".status.robustness", In: []string{"faulted", "degraded"}}, true},
		{"in missing", profile.Rule{Path: ".status.nope", In: []string{"x"}}, false},
		{"notIn", profile.Rule{Path: ".status.robustness", NotIn: []string{"healthy"}}, true},
		{"exists", profile.Rule{Path: ".status.kubernetesStatus.pvcName", Exists: true}, true},
		{"exists missing", profile.Rule{Path: ".status.kubernetesStatus.podName", Exists: true}, false},
		{"exists through a scalar", profile.Rule{Path: ".status.robustness.x", Exists: true}, false},
		{"missing", profile.Rule{Path: "status.lastBackup", Missing: true}, true},
		{"missing present", profile.Rule{Path: "status.state", Missing: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := matchRule(tt.rule, obj); got != tt.match {
				t.Errorf("match = %v, want %v", got, tt.match)
			}
		})
	}
}

func TestK8sResourceCondition(t *testing.T) {
	k := &K8sResource{
		GVR: vmGVR,
		Rules: []profile.Rule{
			{Condition: &profile.ConditionMatch{Type: "RestartRequired", Status: "True"}, Band: "warning"},
			{Condition: &profile.ConditionMatch{Type: "Ready", Status: "false"}, Band: "error"},
		},
	}
	res := runK8sResource(t, k, Prior{},
		vm("ok", cond("Ready", "True")),
		vm("restart", cond("Ready", "True"), cond("RestartRequired", "True", "TemplateChanged")),
		vm("down", cond("Ready", "False", "PodTerminating")),
		vm("none"),
	)
	if res.MinBand != status.Error {
		t.Errorf("band = %s, want error", res.MinBand)
	}
	if want := "4 virtualmachines: 2 ok, 1 warning, 1 error"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if want := "vms/down error: Ready=False (PodTerminating); vms/restart warning: RestartRequired=True (TemplateChanged)"; res.Problem != want {
		t.Errorf("problem = %q, want %q", res.Problem, want)
	}
}

func TestK8sResourceSelector(t *testing.T) {
	other := volume("pvc-2", "faulted", "data-b")
	other.SetNamespace("other")
	labelled := volume("pvc-3", "degraded", "data-c")
	labelled.SetLabels(map[string]string{"team": "infra"})

	res := runK8sResource(t, longhorn(), Prior{}, volume("pvc-1", "healthy", "data-a"), other)
	if res.Summary != "1 volume: 1 ok" {
		t.Errorf("namespace selector not applied: %q", res.Summary)
	}

	all := longhorn()
	all.Namespaces = []string{"*"}
	all.Labels = map[string]string{"team": "infra"}
	res = runK8sResource(t, all, Prior{}, volume("pvc-1", "healthy", "data-a"), other, labelled)
	if res.Summary != "1 volume: 1 error" {
		t.Errorf("label selector not applied: %q", res.Summary)
	}
}

func TestK8sResourceTruncates(t *testing.T) {
	var objs []runtime.Object
	for i := range 13 {
		objs = append(objs, volume(fmt.Sprintf("pvc-%02d", i), "degraded", ""))
	}
	res := runK8sResource(t, longhorn(), Prior{}, objs...)
	if res.Summary != "13 volumes: 13 error" {
		t.Errorf("summary = %q", res.Summary)
	}
	if strings.Count(res.Problem, "error:") != maxListed || !strings.HasSuffix(res.Problem, "; and 3 more") {
		t.Errorf("problem not truncated: %q", res.Problem)
	}
}

func TestK8sResourceMinAge(t *testing.T) {
	k := longhorn()
	k.MinAge = 30 * time.Minute
	objs := []runtime.Object{volume("pvc-1", "degraded", "data-a"), volume("pvc-2", "healthy", "data-b")}

	// First run: bad since now, pending.
	res := runK8sResource(t, k, Prior{}, objs...)
	if res.MinBand != status.OK || res.Summary != "2 volumes: 1 ok, 1 pending" || res.Problem != "" {
		t.Errorf("first run: %s %q %q", res.MinBand, res.Summary, res.Problem)
	}
	if !res.BadSince["longhorn-system/pvc-1"].Equal(srNow) || len(res.BadSince) != 1 {
		t.Errorf("BadSince = %v", res.BadSince)
	}

	// Bad for long enough: counts.
	prior := Prior{BadSince: map[string]time.Time{
		"longhorn-system/pvc-1": srNow.Add(-time.Hour),
		"longhorn-system/pvc-2": srNow.Add(-time.Hour), // healthy again: forgotten
	}}
	res = runK8sResource(t, k, prior, objs...)
	if res.MinBand != status.Error || res.Summary != "2 volumes: 1 ok, 1 error" {
		t.Errorf("after minAge: %s %q", res.MinBand, res.Summary)
	}
	if !res.BadSince["longhorn-system/pvc-1"].Equal(srNow.Add(-time.Hour)) || len(res.BadSince) != 1 {
		t.Errorf("BadSince = %v", res.BadSince)
	}
}

func TestK8sResourceForbidden(t *testing.T) {
	client := newFakeDynamic()
	client.PrependReactor("list", "volumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "", fmt.Errorf("no RBAC"))
	})
	k := longhorn()
	k.Kube = func() (*Kube, error) { return &Kube{Client: client}, nil }
	_, err := k.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listing volumes.longhorn.io") || !apierrors.IsForbidden(err) {
		t.Errorf("err = %v, want a wrapped Forbidden", err)
	}
}

func TestNewChecker_K8sResource(t *testing.T) {
	c, err := New(profile.Check{
		ID: "longhorn", Type: profile.TypeK8sResource,
		Resource:    &profile.Resource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"},
		Selector:    &profile.Selector{Namespaces: []string{"longhorn-system"}},
		Rules:       longhornRules,
		DisplayPath: ".status.kubernetesStatus.pvcName",
		MinAge:      profile.Duration(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := c.(*K8sResource)
	if !ok {
		t.Fatalf("got %T", c)
	}
	if _, ok := c.(PriorChecker); !ok {
		t.Error("K8sResource is not a PriorChecker")
	}
	if k.GVR != volumeGVR || k.MinAge != time.Hour || k.Namespaces[0] != "longhorn-system" || len(k.Rules) != 2 {
		t.Errorf("unexpected checker %+v", k)
	}
}
