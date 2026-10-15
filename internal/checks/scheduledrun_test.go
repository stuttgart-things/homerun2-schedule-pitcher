package checks

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

var (
	srNow      = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	ansibleGVR = schema.GroupVersionResource{Group: "resources.stuttgart-things.com", Version: "v1alpha1", Resource: "ansibleruns"}
	jobGVR     = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

func ts(d time.Duration) string { return srNow.Add(-d).Format(time.RFC3339) }

// scheduledRun is a nightly ScheduledRun of an AnsibleRun in tekton-ci.
func scheduledRun(name string, mut func(spec, st map[string]any)) *unstructured.Unstructured {
	spec := map[string]any{
		"schedule":        "0 2 * * *",
		"targetNamespace": "tekton-ci",
		"manifest": map[string]any{
			"apiVersion": "resources.stuttgart-things.com/v1alpha1",
			"kind":       "AnsibleRun",
		},
	}
	st := map[string]any{
		"lastScheduleTime":   ts(7 * time.Hour),
		"lastSuccessfulTime": ts(7*time.Hour - time.Minute),
	}
	if mut != nil {
		mut(spec, st)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resources.stuttgart-things.com/v1alpha1",
		"kind":       "ScheduledRun",
		"metadata": map[string]any{
			"name": name, "namespace": "default",
			"creationTimestamp": ts(30 * 24 * time.Hour),
			"labels":            map[string]any{"team": "infra"},
		},
		"spec":   spec,
		"status": st,
	}}
	return u
}

// instance is a run created by owner, age ago, with conditions.
func instance(gvr schema.GroupVersionResource, kind, owner, name string, age time.Duration, conds ...map[string]any) *unstructured.Unstructured {
	var cs []any
	for _, c := range conds {
		cs = append(cs, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": "tekton-ci",
			"creationTimestamp": ts(age),
			"labels":            map[string]any{OwnerLabel: owner},
		},
		"status": map[string]any{"conditions": cs},
	}}
}

func cond(typ, st string, reason ...string) map[string]any {
	c := map[string]any{"type": typ, "status": st}
	if len(reason) > 0 {
		c["reason"] = reason[0]
	}
	if len(reason) > 1 {
		c["message"] = reason[1]
	}
	return c
}

func fakeKube(objs ...runtime.Object) *Kube {
	client := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ScheduledRunGVR: "ScheduledRunList",
		ansibleGVR:      "AnsibleRunList",
		jobGVR:          "JobList",
	}, objs...)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(ansibleGVR.GroupVersion().WithKind("AnsibleRun"), meta.RESTScopeNamespace)
	mapper.Add(jobGVR.GroupVersion().WithKind("Job"), meta.RESTScopeNamespace)
	return &Kube{Client: client, Mapper: mapper}
}

func runCheck(t *testing.T, sr *ScheduledRun, objs ...runtime.Object) Result {
	t.Helper()
	kube := fakeKube(objs...)
	sr.Kube = func() (*Kube, error) { return kube, nil }
	sr.Now = func() time.Time { return srNow }
	if sr.RunTimeout == 0 {
		sr.RunTimeout = profile.DefaultRunTimeout
	}
	res, err := sr.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestScheduledRun(t *testing.T) {
	ready := cond("Ready", "True")
	tests := []struct {
		name    string
		objs    []runtime.Object
		band    status.Band
		summary string
		problem string
	}{
		{
			name: "newest run ready",
			objs: []runtime.Object{
				scheduledRun("baseos", nil),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-1", 7*time.Hour, ready),
			},
			band: status.OK, summary: "1 ScheduledRun: 1 ok",
		},
		{
			name: "newest run failed",
			objs: []runtime.Object{
				scheduledRun("baseos", nil),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-2", 7*time.Hour, cond("Succeeded", "False", "Failed", "task setup failed")),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-1", 31*time.Hour, ready),
			},
			band:    status.Error,
			summary: "1 ScheduledRun: 1 failed",
			problem: "default/baseos failed: AnsibleRun baseos-2 failed: Failed: task setup failed",
		},
		{
			name: "older failure, newest ok",
			objs: []runtime.Object{
				scheduledRun("baseos", nil),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-2", 7*time.Hour, ready),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-1", 31*time.Hour, cond("Ready", "False")),
			},
			band: status.OK, summary: "1 ScheduledRun: 1 ok",
		},
		{
			name: "newest still running, previous decides",
			objs: []runtime.Object{
				scheduledRun("baseos", nil),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-2", time.Hour, cond("Ready", "False", "Creating")),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-1", 25*time.Hour, ready),
			},
			band: status.OK, summary: "1 ScheduledRun: 1 ok",
		},
		{
			name: "newest never finished",
			objs: []runtime.Object{
				scheduledRun("baseos", nil),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-2", 7*time.Hour, cond("Ready", "False", "Creating")),
			},
			band:    status.Error,
			summary: "1 ScheduledRun: 1 failed",
			problem: "default/baseos failed: AnsibleRun baseos-2 not done after 7h",
		},
		{
			name: "stale: a tick was missed",
			objs: []runtime.Object{
				scheduledRun("baseos", func(_, st map[string]any) {
					st["lastScheduleTime"] = ts(55 * time.Hour)
					st["lastSuccessfulTime"] = ts(55 * time.Hour)
				}),
			},
			band:    status.Error,
			summary: "1 ScheduledRun: 1 stale",
			problem: "default/baseos stale: last run 2d ago",
		},
		{
			name: "never ran",
			objs: []runtime.Object{
				scheduledRun("baseos", func(_, st map[string]any) {
					delete(st, "lastScheduleTime")
					delete(st, "lastSuccessfulTime")
				}),
			},
			band:    status.Error,
			summary: "1 ScheduledRun: 1 stale",
			problem: "default/baseos stale: never ran since 30d ago",
		},
		{
			name: "create job failed",
			objs: []runtime.Object{
				scheduledRun("baseos", func(_, st map[string]any) {
					st["lastSuccessfulTime"] = ts(31 * time.Hour)
				}),
				instance(ansibleGVR, "AnsibleRun", "baseos", "baseos-1", 31*time.Hour, ready),
			},
			band:    status.Error,
			summary: "1 ScheduledRun: 1 failed",
			problem: "default/baseos failed: creating the run failed 7h ago",
		},
		{
			name: "suspended is not checked",
			objs: []runtime.Object{
				scheduledRun("baseos", func(spec, st map[string]any) {
					spec["suspend"] = true
					st["lastScheduleTime"] = ts(20 * 24 * time.Hour)
				}),
			},
			band: status.OK, summary: "1 ScheduledRun: 1 suspended",
		},
		{
			name: "job target",
			objs: []runtime.Object{
				scheduledRun("heartbeat", func(spec, _ map[string]any) {
					spec["manifest"] = map[string]any{"apiVersion": "batch/v1", "kind": "Job"}
				}),
				instance(jobGVR, "Job", "heartbeat", "heartbeat-1", 7*time.Hour, cond("Failed", "True", "BackoffLimitExceeded")),
			},
			band:    status.Error,
			summary: "1 ScheduledRun: 1 failed",
			problem: "default/heartbeat failed: Job heartbeat-1 failed: BackoffLimitExceeded",
		},
		{
			name: "no instances is fine",
			objs: []runtime.Object{scheduledRun("baseos", nil)},
			band: status.OK, summary: "1 ScheduledRun: 1 ok",
		},
		{
			name: "mixed",
			objs: []runtime.Object{
				scheduledRun("a", nil),
				scheduledRun("b", nil),
				scheduledRun("c", func(spec, _ map[string]any) { spec["suspend"] = true }),
				instance(ansibleGVR, "AnsibleRun", "a", "a-1", 7*time.Hour, ready),
				instance(ansibleGVR, "AnsibleRun", "b", "b-1", 7*time.Hour, cond("Failed", "True")),
			},
			band:    status.Error,
			summary: "3 ScheduledRuns: 1 ok, 1 failed, 1 suspended",
			problem: "default/b failed: AnsibleRun b-1 failed: Failed=True",
		},
		{
			name: "nothing found",
			band: status.Warning, summary: "no ScheduledRun found",
			problem: "no ScheduledRun matches the selector",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runCheck(t, &ScheduledRun{}, tt.objs...)
			if res.MinBand != tt.band {
				t.Errorf("band = %s, want %s", res.MinBand, tt.band)
			}
			if res.Summary != tt.summary {
				t.Errorf("summary = %q, want %q", res.Summary, tt.summary)
			}
			if res.Problem != tt.problem {
				t.Errorf("problem = %q, want %q", res.Problem, tt.problem)
			}
		})
	}
}

func TestScheduledRunSelector(t *testing.T) {
	other := scheduledRun("other", nil)
	other.SetLabels(map[string]string{"team": "apps"})
	failing := instance(ansibleGVR, "AnsibleRun", "other", "other-1", 7*time.Hour, cond("Failed", "True"))

	res := runCheck(t, &ScheduledRun{Labels: map[string]string{"team": "infra"}}, scheduledRun("baseos", nil), other, failing)
	if res.MinBand != status.OK || res.Summary != "1 ScheduledRun: 1 ok" {
		t.Errorf("label selector not applied: %s %q", res.MinBand, res.Summary)
	}

	res = runCheck(t, &ScheduledRun{Namespaces: []string{"apps"}}, scheduledRun("baseos", nil))
	if res.Summary != "no ScheduledRun found" {
		t.Errorf("namespace selector not applied: %q", res.Summary)
	}
}

func TestScheduledRunMaxAge(t *testing.T) {
	// Last tick 7h ago: fine for the nightly schedule, stale with maxAge 6h.
	res := runCheck(t, &ScheduledRun{MaxAge: 6 * time.Hour}, scheduledRun("baseos", nil))
	if res.MinBand != status.Error || !strings.Contains(res.Problem, "stale: last run 7h ago") {
		t.Errorf("maxAge not applied: %s %q", res.MinBand, res.Problem)
	}
}

func TestScheduledRunWeekdaySchedule(t *testing.T) {
	// Weekdays only: on Monday 09:00 the last tick was Friday 07:45. The
	// next expected tick is today 07:45, so one missed tick is Tuesday.
	monday := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	sr := &ScheduledRun{}
	ref := time.Date(2026, 10, 16, 7, 45, 0, 0, time.UTC)
	due, ok := sr.staleAfter("45 7 * * 1-5", ref)
	if !ok || !monday.Before(due) || due.Weekday() != time.Tuesday {
		t.Errorf("staleAfter = %s, %v; want Tuesday after Monday", due, ok)
	}
}

func TestNewChecker_ScheduledRun(t *testing.T) {
	c, err := New(profile.Check{
		ID: "nightly", Type: profile.TypeScheduledRun,
		Selector:   &profile.Selector{Namespaces: []string{"*"}, Labels: map[string]string{"team": "infra"}},
		MaxAge:     profile.Duration(26 * time.Hour),
		RunTimeout: profile.Duration(2 * time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sr, ok := c.(*ScheduledRun)
	if !ok {
		t.Fatalf("got %T", c)
	}
	if sr.MaxAge != 26*time.Hour || sr.RunTimeout != 2*time.Hour || sr.Labels["team"] != "infra" {
		t.Errorf("unexpected checker %+v", sr)
	}
}
