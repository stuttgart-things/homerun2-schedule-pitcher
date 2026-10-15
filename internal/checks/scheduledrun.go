package checks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/secrets"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// ScheduledRunGVR is the Crossplane ScheduledRun XR
// (stuttgart-things/crossplane-configurations cicd/scheduled-run).
var ScheduledRunGVR = schema.GroupVersionResource{
	Group: "resources.stuttgart-things.com", Version: "v1alpha1", Resource: "scheduledruns",
}

// OwnerLabel is stamped on every instance a ScheduledRun creates; its value
// is the ScheduledRun's name.
const OwnerLabel = "scheduled-run.resources.stuttgart-things.com/owner"

// staleGrace is added to the expected tick: a CronJob starts its job with
// some delay.
const staleGrace = 10 * time.Minute

// createGrace: a tick whose create job has not succeeded after this long
// failed. The job only runs kubectl create.
const createGrace = 15 * time.Minute

// Kube is the cluster access of the scheduled-run check.
type Kube struct {
	Client dynamic.Interface
	Mapper meta.RESTMapper
}

// kubeFromEnv builds the cluster access once, on first use, so profiles
// without scheduled-run checks never need a cluster.
var kubeFromEnv = sync.OnceValues(func() (*Kube, error) {
	cfg, err := secrets.KubeConfig()
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating discovery client: %w", err)
	}
	return &Kube{
		Client: client,
		Mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
	}, nil
})

// ScheduledRun watches Crossplane ScheduledRuns: whether they still fire
// (stale) and whether the newest run they created succeeded (failed). All
// matching ScheduledRuns make one result; its band is the worst of them.
type ScheduledRun struct {
	Kube       func() (*Kube, error)
	Namespaces []string
	Labels     map[string]string
	// MaxAge overrides the schedule-derived staleness limit.
	MaxAge     time.Duration
	RunTimeout time.Duration
	Now        func() time.Time
}

// NewScheduledRun builds the check from its profile definition.
func NewScheduledRun(c profile.Check) *ScheduledRun {
	sr := &ScheduledRun{Kube: kubeFromEnv, MaxAge: c.MaxAge.D(), RunTimeout: c.RunTimeout.D()}
	if c.Selector != nil {
		sr.Namespaces = c.Selector.Namespaces
		sr.Labels = c.Selector.Labels
	}
	return sr
}

// runVerdict is what was found about one ScheduledRun.
type runVerdict struct {
	name    string
	verdict string // ok, failed, stale, suspended
	detail  string
}

func (v runVerdict) band() status.Band {
	switch v.verdict {
	case "failed", "stale":
		return status.Error
	default:
		return status.OK
	}
}

func (s *ScheduledRun) Run(ctx context.Context) (Result, error) {
	kube, err := s.Kube()
	if err != nil {
		return Result{}, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	namespaces := s.Namespaces
	if len(namespaces) == 0 || slices.Contains(namespaces, "*") {
		namespaces = []string{metav1.NamespaceAll}
	}
	opts := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(s.Labels).String()}

	var verdicts []runVerdict
	for _, ns := range namespaces {
		list, err := kube.Client.Resource(ScheduledRunGVR).Namespace(ns).List(ctx, opts)
		if err != nil {
			return Result{}, fmt.Errorf("listing ScheduledRuns: %w", err)
		}
		for i := range list.Items {
			v, err := s.evaluate(ctx, kube, &list.Items[i], now)
			if err != nil {
				return Result{}, err
			}
			verdicts = append(verdicts, v)
		}
	}
	return summarize(verdicts), nil
}

// summarize turns the verdicts into one result, e.g. "6 ScheduledRuns: 4 ok,
// 1 failed, 1 suspended" with the failed and stale ones as the problem.
func summarize(verdicts []runVerdict) Result {
	if len(verdicts) == 0 {
		return Result{
			MinBand: status.Warning,
			Problem: "no ScheduledRun matches the selector",
			Summary: "no ScheduledRun found",
		}
	}
	slices.SortFunc(verdicts, func(a, b runVerdict) int { return strings.Compare(a.name, b.name) })
	counts := map[string]int{}
	band := status.OK
	var problems []string
	for _, v := range verdicts {
		counts[v.verdict]++
		band = status.Max(band, v.band())
		if v.band() > status.OK {
			problems = append(problems, fmt.Sprintf("%s %s: %s", v.name, v.verdict, v.detail))
		}
	}
	var parts []string
	for _, k := range []string{"ok", "failed", "stale", "suspended"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	noun := "ScheduledRuns"
	if len(verdicts) == 1 {
		noun = "ScheduledRun"
	}
	return Result{
		MinBand: band,
		Problem: strings.Join(problems, "; "),
		Summary: fmt.Sprintf("%d %s: %s", len(verdicts), noun, strings.Join(parts, ", ")),
	}
}

// evaluate checks one ScheduledRun: suspended, stale, its last create job,
// then the newest instance it created.
func (s *ScheduledRun) evaluate(ctx context.Context, kube *Kube, xr *unstructured.Unstructured, now time.Time) (runVerdict, error) {
	v := runVerdict{name: xr.GetNamespace() + "/" + xr.GetName(), verdict: "ok"}

	if suspended, _, _ := unstructured.NestedBool(xr.Object, "spec", "suspend"); suspended {
		v.verdict = "suspended"
		return v, nil
	}

	lastSchedule := timeField(xr, "status", "lastScheduleTime")
	lastSuccess := timeField(xr, "status", "lastSuccessfulTime")
	ref := lastSchedule
	if ref.IsZero() {
		ref = xr.GetCreationTimestamp().Time
	}
	schedule, _, _ := unstructured.NestedString(xr.Object, "spec", "schedule")
	if due, ok := s.staleAfter(schedule, ref); ok && now.After(due) {
		v.verdict = "stale"
		if lastSchedule.IsZero() {
			v.detail = fmt.Sprintf("never ran since %s", ago(now, ref))
		} else {
			v.detail = fmt.Sprintf("last run %s", ago(now, ref))
		}
		return v, nil
	}

	// The CronJob's job only creates the instance; when it did not succeed,
	// no run was created at all.
	if !lastSchedule.IsZero() && lastSchedule.After(lastSuccess) && now.Sub(lastSchedule) > createGrace {
		v.verdict = "failed"
		v.detail = fmt.Sprintf("creating the run failed %s", ago(now, lastSchedule))
		return v, nil
	}

	detail, failed, err := s.newestInstance(ctx, kube, xr, now)
	if err != nil {
		return v, fmt.Errorf("ScheduledRun %s: %w", v.name, err)
	}
	if failed {
		v.verdict = "failed"
		v.detail = detail
	}
	return v, nil
}

// staleAfter is when a ScheduledRun whose last tick was at ref is stale:
// ref + maxAge, or one missed tick of its schedule plus a grace. ok is false
// when neither is known.
func (s *ScheduledRun) staleAfter(schedule string, ref time.Time) (time.Time, bool) {
	if s.MaxAge > 0 {
		return ref.Add(s.MaxAge), true
	}
	// CronJobs without spec.timeZone run in the controller manager's zone,
	// usually UTC; the composition sets none.
	sched, err := profile.CronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, false
	}
	return sched.Next(sched.Next(ref.UTC())).Add(staleGrace), true
}

// newestInstance looks at the instances the ScheduledRun created, newest
// first. An unfinished instance younger than RunTimeout is skipped in favour
// of the one before it; an older one counts as failed. failed reports the
// verdict on the newest decided instance; none at all is not a failure (a
// target may delete itself, e.g. a Job with a TTL).
func (s *ScheduledRun) newestInstance(ctx context.Context, kube *Kube, xr *unstructured.Unstructured, now time.Time) (detail string, failed bool, err error) {
	apiVersion, _, _ := unstructured.NestedString(xr.Object, "spec", "manifest", "apiVersion")
	kind, _, _ := unstructured.NestedString(xr.Object, "spec", "manifest", "kind")
	if apiVersion == "" || kind == "" {
		return "", false, errors.New("spec.manifest has no apiVersion or kind")
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return "", false, fmt.Errorf("spec.manifest.apiVersion: %w", err)
	}
	mapping, err := restMapping(kube.Mapper, gv.WithKind(kind))
	if err != nil {
		return "", false, fmt.Errorf("resolving %s %s: %w", apiVersion, kind, err)
	}

	ri := kube.Client.Resource(mapping.Resource)
	opts := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{OwnerLabel: xr.GetName()}).String()}
	var list *unstructured.UnstructuredList
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		list, err = ri.Namespace(targetNamespace(xr)).List(ctx, opts)
	} else {
		list, err = ri.List(ctx, opts)
	}
	if err != nil {
		return "", false, fmt.Errorf("listing %s: %w", mapping.Resource.Resource, err)
	}

	items := list.Items
	slices.SortFunc(items, func(a, b unstructured.Unstructured) int {
		return b.GetCreationTimestamp().Compare(a.GetCreationTimestamp().Time)
	})
	for i := range items {
		it := &items[i]
		outcome, why := instanceOutcome(it)
		name := kind + " " + it.GetName()
		switch outcome {
		case "succeeded":
			return "", false, nil
		case "failed":
			return fmt.Sprintf("%s failed: %s", name, why), true, nil
		default:
			age := now.Sub(it.GetCreationTimestamp().Time)
			if age > s.RunTimeout {
				return fmt.Sprintf("%s not done after %s", name, roundDuration(age)), true, nil
			}
		}
	}
	return "", false, nil
}

func restMapping(m meta.RESTMapper, gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mapping, err := m.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil && meta.IsNoMatchError(err) {
		// A CRD installed after the cache was filled.
		if r, ok := m.(meta.ResettableRESTMapper); ok {
			r.Reset()
			mapping, err = m.RESTMapping(gvk.GroupKind(), gvk.Version)
		}
	}
	return mapping, err
}

// targetNamespace mirrors the composition: targetNamespace, else namespace,
// else crossplane-system.
func targetNamespace(xr *unstructured.Unstructured) string {
	for _, f := range []string{"targetNamespace", "namespace"} {
		if ns, _, _ := unstructured.NestedString(xr.Object, "spec", f); ns != "" {
			return ns
		}
	}
	return "crossplane-system"
}

// instanceOutcome reads the conditions of a created instance: a Job
// (Complete / Failed), a Tekton run (Succeeded) or a Crossplane XR (Ready).
// It returns succeeded, failed or pending, and for failed the reason.
func instanceOutcome(obj *unstructured.Unstructured) (outcome, why string) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	byType := map[string]map[string]any{}
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok {
			if t, ok := m["type"].(string); ok {
				byType[t] = m
			}
		}
	}
	is := func(t, want string) bool {
		c, ok := byType[t]
		return ok && c["status"] == want
	}
	reason := func(t string) string {
		c := byType[t]
		r, _ := c["reason"].(string)
		msg, _ := c["message"].(string)
		switch {
		case r != "" && msg != "":
			return r + ": " + truncate(msg, 120)
		case r != "":
			return r
		default:
			return t + "=" + fmt.Sprint(c["status"])
		}
	}
	switch {
	case is("Failed", "True"):
		return "failed", reason("Failed")
	case is("Succeeded", "False"):
		return "failed", reason("Succeeded")
	case is("Complete", "True"), is("Succeeded", "True"):
		return "succeeded", ""
	case is("Ready", "True") && byType["Succeeded"] == nil:
		return "succeeded", ""
	default:
		return "pending", ""
	}
}

func timeField(obj *unstructured.Unstructured, fields ...string) time.Time {
	s, _, _ := unstructured.NestedString(obj.Object, fields...)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func ago(now, t time.Time) string {
	return roundDuration(now.Sub(t)) + " ago"
}

// roundDuration renders hours, or days from two days on.
func roundDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
