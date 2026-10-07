// Package discovery turns labelled Kubernetes Secrets into checks.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/profile"
)

// Annotations that refine a discovered check.
const (
	AnnotationPrefix      = "homerun2.sthings.io/"
	AnnotationCheckID     = AnnotationPrefix + "check-id"
	AnnotationCheckType   = AnnotationPrefix + "check-type"
	AnnotationTokenKey    = AnnotationPrefix + "token-key"
	AnnotationDescription = AnnotationPrefix + "description"
	AnnotationURL         = AnnotationPrefix + "url"
	AnnotationTags        = AnnotationPrefix + "tags"
	AnnotationAssignee    = AnnotationPrefix + "assignee"
	AnnotationAPIURL      = AnnotationPrefix + "api-url"
	// vault-token-ttl
	AnnotationVaultAddr      = AnnotationPrefix + "vault-addr"
	AnnotationVaultNamespace = AnnotationPrefix + "vault-namespace"
	AnnotationCAFile         = AnnotationPrefix + "ca-file"

	// LabelWatch selects Secrets; its value names the token kind:
	// true or github-token (GitHub), vault-token (Vault/OpenBao).
	LabelWatch = AnnotationPrefix + "watch-expiry"
)

// TokenKeys are tried in this order when a Secret has several keys and no
// token-key annotation: Tekton/Backstage use GITHUB_TOKEN, Flux git-token-auth
// and Argo CD repository secrets use password.
var TokenKeys = []string{"token", "GITHUB_TOKEN", "github_token", "password"}

// VaultTokenKeys are tried for Vault tokens.
var VaultTokenKeys = []string{"token", "VAULT_TOKEN", "vault-token", "vault_token"}

// Discoverer lists labelled Secrets.
type Discoverer struct {
	Client  kubernetes.Interface
	Config  profile.Discovery
	Profile *profile.SchedulePitcherProfile
}

// Discover returns one check per labelled Secret. Secrets that cannot be
// turned into a valid check are skipped and reported in the error; the
// returned checks are still usable. complete is false when a namespace could
// not be listed: the result then misses checks and must not be used to
// remove any.
func (d *Discoverer) Discover(ctx context.Context) (checks []profile.Check, complete bool, err error) {
	namespaces := d.Config.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	var out []profile.Check
	var errs []error
	complete = true
	seen := map[string]string{}
	for _, ns := range namespaces {
		list, err := d.Client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: d.Config.LabelSelector})
		if err != nil {
			errs = append(errs, fmt.Errorf("listing secrets in %s: %w", nsName(ns), err))
			complete = false
			continue
		}
		for _, s := range list.Items {
			keys := make([]string, 0, len(s.Data))
			for k := range s.Data {
				keys = append(keys, k)
			}
			c, err := checkFor(s.Namespace, s.Name, s.Labels, s.Annotations, keys)
			if err == nil {
				c, err = d.Profile.CompleteCheck(c)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("secret %s/%s: %w", s.Namespace, s.Name, err))
				continue
			}
			if other, dup := seen[c.ID]; dup {
				errs = append(errs, fmt.Errorf("secret %s/%s: check id %q already used by %s", s.Namespace, s.Name, c.ID, other))
				continue
			}
			seen[c.ID] = s.Namespace + "/" + s.Name
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b profile.Check) int { return strings.Compare(a.ID, b.ID) })
	return out, complete, errors.Join(errs...)
}

// Syncer is what Loop needs from the scheduler.
type Syncer interface {
	Sync(checks []profile.Check) error
}

// Loop scans now and then every interval, and hands the profile checks plus
// the discovered ones to the scheduler. An incomplete scan only adds checks.
func (d *Discoverer) Loop(ctx context.Context, s Syncer, current func() []profile.Check, interval time.Duration) {
	d.Once(ctx, s, current)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Once(ctx, s, current)
		}
	}
}

// Once runs one scan and syncs the result.
func (d *Discoverer) Once(ctx context.Context, s Syncer, current func() []profile.Check) {
	found, complete, err := d.Discover(ctx)
	if err != nil {
		slog.Warn("discovery problems", "error", err)
	}
	if !complete {
		// Keep what is known; only add.
		known := map[string]bool{}
		for _, c := range current() {
			known[c.ID] = true
		}
		var add []profile.Check
		for _, c := range found {
			if !known[c.ID] {
				add = append(add, c)
			}
		}
		found = append(discovered(current()), add...)
	}
	if err := s.Sync(Merge(d.Profile.Spec.Checks, found)); err != nil {
		slog.Error("syncing discovered checks failed", "error", err)
	}
	slog.Info("discovery done", "discovered", len(found), "complete", complete)
}

func discovered(cs []profile.Check) []profile.Check {
	var out []profile.Check
	for _, c := range cs {
		if c.Origin == profile.OriginDiscovered {
			out = append(out, c)
		}
	}
	return out
}

func nsName(ns string) string {
	if ns == metav1.NamespaceAll {
		return "all namespaces"
	}
	return ns
}

// checkFor builds the check of one Secret from its label, annotations and keys.
func checkFor(namespace, name string, lbl, ann map[string]string, keys []string) (profile.Check, error) {
	typ := ann[AnnotationCheckType]
	if typ == "" {
		switch lbl[LabelWatch] {
		case "vault-token":
			typ = profile.TypeVaultTokenTTL
		default:
			typ = profile.TypeGitHubTokenExpiry
		}
	}
	candidates := TokenKeys
	switch typ {
	case profile.TypeGitHubTokenExpiry:
	case profile.TypeVaultTokenTTL:
		candidates = VaultTokenKeys
		if ann[AnnotationVaultAddr] == "" {
			return profile.Check{}, fmt.Errorf("vault token needs the annotation %s", AnnotationVaultAddr)
		}
	default:
		return profile.Check{}, fmt.Errorf("check type %q cannot be discovered (only %s, %s)", typ, profile.TypeGitHubTokenExpiry, profile.TypeVaultTokenTTL)
	}
	key, err := tokenKey(ann[AnnotationTokenKey], keys, candidates)
	if err != nil {
		return profile.Check{}, err
	}
	id := ann[AnnotationCheckID]
	if id == "" {
		id = namespace + "." + name
	}
	c := profile.Check{
		ID:          id,
		Type:        typ,
		Description: ann[AnnotationDescription],
		URL:         ann[AnnotationURL],
		Assignee:    ann[AnnotationAssignee],
		APIURL:      ann[AnnotationAPIURL],
		Addr:        ann[AnnotationVaultAddr],
		CAFile:      ann[AnnotationCAFile],
		Origin:      profile.OriginDiscovered,
		TokenFrom: &profile.ValueFrom{SecretKeyRef: &profile.SecretKeyRef{
			Namespace: namespace, Name: name, Key: key,
		}},
	}
	c.VaultNamespace = ann[AnnotationVaultNamespace]
	if c.Description == "" {
		c.Description = fmt.Sprintf("Discovered: Secret %s/%s, key %s", namespace, name, key)
	}
	for _, t := range strings.Split(ann[AnnotationTags], ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.Tags = append(c.Tags, t)
		}
	}
	return c, nil
}

func tokenKey(annotated string, keys, candidates []string) (string, error) {
	if annotated != "" {
		if !slices.Contains(keys, annotated) {
			return "", fmt.Errorf("annotated token key %q not found (keys: %s)", annotated, strings.Join(sorted(keys), ", "))
		}
		return annotated, nil
	}
	if len(keys) == 1 {
		return keys[0], nil
	}
	for _, k := range candidates {
		if slices.Contains(keys, k) {
			return k, nil
		}
	}
	return "", fmt.Errorf("cannot tell which key holds the token (keys: %s); set %s", strings.Join(sorted(keys), ", "), AnnotationTokenKey)
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// Merge adds discovered checks to the profile checks; a profile check wins
// over a discovered one with the same id.
func Merge(own, discovered []profile.Check) []profile.Check {
	out := slices.Clone(own)
	ids := map[string]bool{}
	for _, c := range own {
		ids[c.ID] = true
	}
	for _, c := range discovered {
		if ids[c.ID] {
			slog.Info("discovered check shadowed by the profile", "check", c.ID)
			continue
		}
		out = append(out, c)
	}
	return out
}
