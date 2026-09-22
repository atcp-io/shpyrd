package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	shpyrdv1 "shpyrd/api/v1alpha1"
)

// maxReleases bounds the history kept in status.
const maxReleases = 20

// configHash fingerprints everything that changes the running configuration
// without changing the image: plain env vars, the <app>-env Secret and the
// instance size of each process type.
func configHash(app *shpyrdv1.App, secret *corev1.Secret, sizes map[string]string) string {
	h := sha256.New()
	sizeNames := make([]string, 0, len(sizes))
	for k := range sizes {
		sizeNames = append(sizeNames, k)
	}
	sort.Strings(sizeNames)
	for _, k := range sizeNames {
		h.Write([]byte("size:" + k + "=" + sizes[k]))
		h.Write([]byte{0})
	}
	env := append([]corev1.EnvVar(nil), app.Spec.Env...)
	sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })
	for _, e := range env {
		b, _ := json.Marshal(e)
		h.Write(b)
		h.Write([]byte{0})
	}
	if secret != nil {
		keys := make([]string, 0, len(secret.Data))
		for k := range secret.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{'='})
			h.Write(secret.Data[k])
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// buildState summarises a kpack Image status.
type buildState struct {
	// LatestImage is the newest successfully built image (may be from an
	// older source while a new build runs).
	LatestImage string
	// LatestBuild is the Build producing the newest image or in progress.
	LatestBuild string
	// Ready mirrors the kpack Ready condition: "True", "False" or "Unknown".
	Ready   string
	Message string
}

func readBuildState(img *unstructured.Unstructured) buildState {
	st := buildState{Ready: "Unknown"}
	if img == nil {
		return st
	}
	st.LatestImage, _, _ = unstructured.NestedString(img.Object, "status", "latestImage")
	st.LatestBuild, _, _ = unstructured.NestedString(img.Object, "status", "latestBuildRef")
	conds, _, _ := unstructured.NestedSlice(img.Object, "status", "conditions")
	for _, raw := range conds {
		m, ok := raw.(map[string]interface{})
		if !ok || m["type"] != "Ready" {
			continue
		}
		if s, ok := m["status"].(string); ok && s != "" {
			st.Ready = s
		}
		if msg, ok := m["message"].(string); ok {
			st.Message = msg
		}
	}
	// A new generation not yet observed means a build is about to start.
	if observed, found, _ := unstructured.NestedInt64(img.Object, "status", "observedGeneration"); found && observed < img.GetGeneration() {
		st.Ready = "Unknown"
		st.Message = "build pending"
	}
	return st
}

// sourceID describes the source of a release for humans: git commit, blob
// ref or archive hash.
func sourceID(app *shpyrdv1.App, build *unstructured.Unstructured) string {
	if app.Spec.Source != nil {
		if b := app.Spec.Source.Blob; b != nil {
			if b.Ref != "" {
				return short(b.Ref)
			}
			if b.SHA256 != "" {
				return "archive " + short(b.SHA256)
			}
		}
	}
	if build != nil {
		if rev, _, _ := unstructured.NestedString(build.Object, "spec", "source", "git", "revision"); rev != "" {
			return short(rev)
		}
	}
	if app.Spec.Source != nil && app.Spec.Source.Git != nil {
		return app.Spec.Source.Git.Revision
	}
	return ""
}

func short(s string) string {
	if strings.HasSuffix(s, "-dirty") {
		return short(strings.TrimSuffix(s, "-dirty")) + "-dirty"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// recordRelease appends a release when image, config or sizes changed. It
// returns true when the history was modified. configDesc describes a
// config-only change (see describeConfigChange); size changes are described
// from the previous release's sizes.
func recordRelease(app *shpyrdv1.App, image, hash, source string, now metav1.Time, configDesc string, sizes map[string]string) bool {
	cur := app.CurrentRelease()
	if cur != nil && cur.Image == image && cur.ConfigHash == hash {
		return false
	}
	desc := app.Annotations[shpyrdv1.AnnotationReleaseNote]
	if desc == "" {
		switch {
		case cur == nil:
			desc = "Initial deploy"
		case cur.Image != image:
			desc = "Deploy"
		case describeSizeChange(cur.Sizes, sizes) != "":
			desc = describeSizeChange(cur.Sizes, sizes)
		default:
			desc = firstNonEmptyStr(configDesc, "Config change")
		}
		if source != "" && (cur == nil || cur.Image != image) {
			desc += " " + source
		}
	}
	n := 1
	if cur != nil {
		n = cur.Number + 1
	}
	app.Status.Releases = append(app.Status.Releases, shpyrdv1.Release{
		Number:      n,
		Image:       image,
		Source:      source,
		ConfigHash:  hash,
		Description: desc,
		CreatedAt:   now,
		Sizes:       sizes,
	})
	if len(app.Status.Releases) > maxReleases {
		app.Status.Releases = app.Status.Releases[len(app.Status.Releases)-maxReleases:]
	}
	return true
}

// setCondition upserts a condition with the App's generation.
func setCondition(app *shpyrdv1.App, condType string, status metav1.ConditionStatus, reason, message string) {
	if len(message) > 1024 {
		message = message[:1024]
	}
	c := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range app.Status.Conditions {
		if app.Status.Conditions[i].Type == condType {
			if app.Status.Conditions[i].Status == status {
				c.LastTransitionTime = app.Status.Conditions[i].LastTransitionTime
			}
			app.Status.Conditions[i] = c
			return
		}
	}
	app.Status.Conditions = append(app.Status.Conditions, c)
}

// failingSummary reports whether any process has instances that cannot
// start, with a message naming the first reason.
func failingSummary(procs map[string]shpyrdv1.ProcessStatus) (bool, string) {
	names := make([]string, 0, len(procs))
	for n := range procs {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		p := procs[n]
		if p.Failing == 0 {
			continue
		}
		msg := fmt.Sprintf("%s: %d instance(s) failing", n, p.Failing)
		if p.Reason != "" {
			msg += " - " + p.Reason
		}
		if p.Ready > 0 {
			msg += fmt.Sprintf(" (%d previous instance(s) still serving)", p.Ready)
		}
		parts = append(parts, msg)
	}
	return len(parts) > 0, strings.Join(parts, "; ")
}

// summarizeProcesses derives the phase message from process rollouts. While
// a rollout runs the message counts instances on the new release ("updated")
// because ready instances of the previous release keep serving until then.
func summarizeProcesses(procs map[string]shpyrdv1.ProcessStatus) (ready bool, msg string) {
	ready = true
	var parts []string
	names := make([]string, 0, len(procs))
	for n := range procs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := procs[n]
		switch {
		case p.Updated < p.Desired:
			ready = false
			parts = append(parts, fmt.Sprintf("%s %d/%d updated", n, p.Updated, p.Desired))
		case p.Ready < p.Desired:
			ready = false
			parts = append(parts, fmt.Sprintf("%s %d/%d ready", n, p.Ready, p.Desired))
		default:
			parts = append(parts, fmt.Sprintf("%s %d/%d", n, p.Ready, p.Desired))
		}
	}
	return ready, strings.Join(parts, " · ")
}

// describeConfigChange names the config vars that differ between two
// snapshots, Heroku style: "Set GREETING", "Remove FOO", "Set A, B".
func describeConfigChange(prev, cur map[string][]byte) string {
	var set, removed []string
	for k, v := range cur {
		if pv, ok := prev[k]; !ok || string(pv) != string(v) {
			set = append(set, k)
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(set)
	sort.Strings(removed)
	var parts []string
	if len(set) > 0 {
		parts = append(parts, "Set "+joinMax(set, 3))
	}
	if len(removed) > 0 {
		parts = append(parts, "Remove "+joinMax(removed, 3))
	}
	if len(parts) == 0 {
		return "Config change"
	}
	return strings.Join(parts, ", ") + " config var" + plural(len(set)+len(removed))
}

// describeSizeChange names processes whose instance size changed:
// "Resize web to shared-m, worker to shared-xs".
func describeSizeChange(prev, cur map[string]string) string {
	if prev == nil {
		return ""
	}
	names := make([]string, 0, len(cur))
	for k := range cur {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts []string
	for _, k := range names {
		if prev[k] != cur[k] {
			parts = append(parts, k+" to "+cur[k])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Resize " + strings.Join(parts, ", ")
}

func joinMax(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" and %d more", len(items)-max)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
