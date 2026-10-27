package checks

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/status"
)

// maxListed is how many bad objects the problem names; the rest are counted.
const maxListed = 10

// K8sResource lists the objects of one resource and reports them as one
// result: an object is bad when one of the rules matches, its band is the
// worst of the matching rules, and the result is the worst object.
type K8sResource struct {
	Kube       func() (*Kube, error)
	GVR        schema.GroupVersionResource
	Namespaces []string
	Labels     map[string]string
	Rules      []profile.Rule
	// DisplayPath names an object in the problem instead of namespace/name.
	DisplayPath string
	// MinAge: an object must have been bad this long (across runs, from the
	// check state) before it counts.
	MinAge time.Duration
	Now    func() time.Time
}

// NewK8sResource builds the check from its profile definition.
func NewK8sResource(c profile.Check) *K8sResource {
	k := &K8sResource{Kube: kubeFromEnv, Rules: c.Rules, DisplayPath: c.DisplayPath, MinAge: c.MinAge.D()}
	if c.Resource != nil {
		k.GVR = schema.GroupVersionResource{Group: c.Resource.Group, Version: c.Resource.Version, Resource: c.Resource.Resource}
	}
	if c.Selector != nil {
		k.Namespaces = c.Selector.Namespaces
		k.Labels = c.Selector.Labels
	}
	return k
}

// objectVerdict is what was found about one object.
type objectVerdict struct {
	name string
	band status.Band
	// pending: bad, but not for minAge yet.
	pending bool
	why     string
}

func (v objectVerdict) label() string {
	if v.pending {
		return "pending"
	}
	return v.band.String()
}

func (k *K8sResource) Run(ctx context.Context) (Result, error) {
	return k.RunWithPrior(ctx, Prior{})
}

// RunWithPrior evaluates the objects; prior.BadSince tells since when an
// object has been bad, for minAge.
func (k *K8sResource) RunWithPrior(ctx context.Context, prior Prior) (Result, error) {
	kube, err := k.Kube()
	if err != nil {
		return Result{}, err
	}
	now := time.Now()
	if k.Now != nil {
		now = k.Now()
	}
	namespaces := k.Namespaces
	if len(namespaces) == 0 || slices.Contains(namespaces, "*") {
		namespaces = []string{metav1.NamespaceAll}
	}
	opts := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(k.Labels).String()}

	var verdicts []objectVerdict
	badSince := map[string]time.Time{}
	for _, ns := range namespaces {
		list, err := kube.Client.Resource(k.GVR).Namespace(ns).List(ctx, opts)
		if err != nil {
			return Result{}, fmt.Errorf("listing %s: %w", k.resourceName(), err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			v := objectVerdict{name: k.display(obj)}
			v.band, v.why = evaluateRules(k.Rules, obj)
			if v.band > status.OK && k.MinAge > 0 {
				key := objectKey(obj)
				since, ok := prior.BadSince[key]
				if !ok || since.After(now) {
					since = now
				}
				badSince[key] = since
				v.pending = now.Sub(since) < k.MinAge
			}
			verdicts = append(verdicts, v)
		}
	}
	res := k.summarize(verdicts)
	if k.MinAge > 0 {
		res.BadSince = badSince
	}
	return res, nil
}

// summarize turns the verdicts into one result, e.g. "10 volumes: 9 ok,
// 1 error" with the bad objects, worst first, as the problem.
func (k *K8sResource) summarize(verdicts []objectVerdict) Result {
	if len(verdicts) == 0 {
		return Result{
			MinBand: status.Warning,
			Problem: fmt.Sprintf("no %s matches the selector", k.resourceName()),
			Summary: fmt.Sprintf("no %s found", k.resourceName()),
		}
	}
	slices.SortFunc(verdicts, func(a, b objectVerdict) int {
		return cmp.Or(cmp.Compare(b.band, a.band), strings.Compare(a.name, b.name))
	})
	counts := map[string]int{}
	band := status.OK
	var problems []string
	for _, v := range verdicts {
		counts[v.label()]++
		if v.band == status.OK || v.pending {
			continue
		}
		band = status.Max(band, v.band)
		problems = append(problems, fmt.Sprintf("%s %s: %s", v.name, v.band, v.why))
	}
	if len(problems) > maxListed {
		problems = append(problems[:maxListed], fmt.Sprintf("and %d more", len(problems)-maxListed))
	}
	var parts []string
	for _, l := range []string{"ok", "pending", "warning", "error", "critical"} {
		if counts[l] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[l], l))
		}
	}
	return Result{
		MinBand: band,
		Problem: strings.Join(problems, "; "),
		Summary: fmt.Sprintf("%d %s: %s", len(verdicts), noun(k.GVR.Resource, len(verdicts)), strings.Join(parts, ", ")),
	}
}

// resourceName renders the resource like kubectl, e.g. volumes.longhorn.io.
func (k *K8sResource) resourceName() string {
	return profile.Resource{Group: k.GVR.Group, Resource: k.GVR.Resource}.String()
}

// display is the value at DisplayPath, else namespace/name.
func (k *K8sResource) display(obj *unstructured.Unstructured) string {
	if k.DisplayPath != "" {
		if v, ok := lookupPath(obj.Object, k.DisplayPath); ok {
			if s := valueString(v); s != "" {
				return s
			}
		}
	}
	return objectKey(obj)
}

func objectKey(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}

// evaluateRules returns the worst band of the matching rules (OK if none
// matches) and what matched.
func evaluateRules(rules []profile.Rule, obj *unstructured.Unstructured) (status.Band, string) {
	band := status.OK
	var whys []string
	for _, r := range rules {
		matched, why := matchRule(r, obj)
		if !matched {
			continue
		}
		band = status.Max(band, status.Parse(r.Band))
		if !slices.Contains(whys, why) {
			whys = append(whys, why)
		}
	}
	return band, strings.Join(whys, ", ")
}

// matchRule reports whether r matches obj and, if so, why. A missing field
// matches notEquals and notIn, like label selectors.
func matchRule(r profile.Rule, obj *unstructured.Unstructured) (bool, string) {
	if r.Condition != nil {
		return matchCondition(r.Condition, obj)
	}
	v, ok := lookupPath(obj.Object, r.Path)
	s := valueString(v)
	why := r.Path + " missing"
	if ok {
		why = r.Path + "=" + s
	}
	var matched bool
	switch {
	case r.Equals != nil:
		matched = ok && s == *r.Equals
	case r.NotEquals != nil:
		matched = !ok || s != *r.NotEquals
	case r.In != nil:
		matched = ok && slices.Contains(r.In, s)
	case r.NotIn != nil:
		matched = !ok || !slices.Contains(r.NotIn, s)
	case r.Exists:
		matched = ok
	case r.Missing:
		matched = !ok
	}
	return matched, why
}

// matchCondition matches an entry of .status.conditions[] by type and
// status (case-insensitive, "False" and false alike).
func matchCondition(c *profile.ConditionMatch, obj *unstructured.Unstructured) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok || valueString(m["type"]) != c.Type {
			continue
		}
		st := valueString(m["status"])
		if !strings.EqualFold(st, c.Status) {
			return false, ""
		}
		why := c.Type + "=" + st
		if reason := valueString(m["reason"]); reason != "" {
			why += " (" + reason + ")"
		}
		return true, why
	}
	return false, ""
}

// lookupPath walks a dot-separated field path; a null value counts as
// missing.
func lookupPath(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for _, seg := range profile.PathSegments(path) {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, cur != nil
}

func valueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// noun is the plural resource name, made singular for one object.
func noun(plural string, n int) string {
	if n != 1 {
		return plural
	}
	switch {
	case strings.HasSuffix(plural, "ies"):
		return strings.TrimSuffix(plural, "ies") + "y"
	case strings.HasSuffix(plural, "sses"), strings.HasSuffix(plural, "xes"), strings.HasSuffix(plural, "ches"):
		return strings.TrimSuffix(plural, "es")
	default:
		return strings.TrimSuffix(plural, "s")
	}
}
