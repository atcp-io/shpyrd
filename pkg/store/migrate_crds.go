package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Teams and project members were Kubernetes objects before RFC-0033. On the
// first start with an empty store, ImportCRDs copies them in and marks the
// objects as migrated; the API stops reading them from then on and a later
// release removes the definitions. Idempotent: an object annotated
// shpyrd.io/migrated is skipped, a store that already has teams or grants is
// left alone.

const migratedAnnotation = "shpyrd.io/migrated"

var (
	teamsGVR   = schema.GroupVersionResource{Group: "shpyrd.io", Version: "v1alpha1", Resource: "teams"}
	membersGVR = schema.GroupVersionResource{Group: "shpyrd.io", Version: "v1alpha1", Resource: "projectmembers"}
)

// ImportCRDs returns how many teams and grants it copied.
func ImportCRDs(ctx context.Context, dyn dynamic.Interface, st Store, ws string) (teams, grants int, err error) {
	existing, err := st.ListTeams(ctx, ws)
	if err != nil {
		return 0, 0, err
	}
	existingGrants, err := st.ListGrants(ctx, ws)
	if err != nil {
		return 0, 0, err
	}
	if len(existing) > 0 || len(existingGrants) > 0 {
		return 0, 0, nil
	}
	teamList, err := dyn.Resource(teamsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, nil // definitions absent: nothing to import
	}
	memberList, err := dyn.Resource(membersGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		memberList = &unstructured.UnstructuredList{}
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	for _, u := range teamList.Items {
		if u.GetAnnotations()[migratedAnnotation] != "" {
			continue
		}
		members, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "members")
		groups, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "groups")
		desc, _, _ := unstructured.NestedString(u.Object, "spec", "description")
		role, _, _ := unstructured.NestedString(u.Object, "spec", "platformRole")
		if _, _, err := st.PutTeam(ctx, ws, Team{Name: u.GetName(), Description: desc, Members: members, Groups: groups, PlatformRole: role}); err != nil {
			return teams, grants, fmt.Errorf("team %s: %w", u.GetName(), err)
		}
		teams++
		mark(ctx, dyn, teamsGVR, &u, stamp)
	}
	for _, u := range memberList.Items {
		if u.GetAnnotations()[migratedAnnotation] != "" {
			continue
		}
		project, _, _ := unstructured.NestedString(u.Object, "spec", "project")
		role, _, _ := unstructured.NestedString(u.Object, "spec", "role")
		user, _, _ := unstructured.NestedString(u.Object, "spec", "user")
		team, _, _ := unstructured.NestedString(u.Object, "spec", "team")
		_, err := st.AddGrant(ctx, ws, Grant{Project: project, Role: role, User: user, Team: team})
		switch {
		case err == nil:
			grants++
		case err == ErrConflict, err == ErrNotFound:
			// duplicate, or a grant to a team that no longer exists
		default:
			return teams, grants, fmt.Errorf("member %s: %w", u.GetName(), err)
		}
		mark(ctx, dyn, membersGVR, &u, stamp)
	}
	return teams, grants, nil
}

func mark(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, u *unstructured.Unstructured, stamp string) {
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[migratedAnnotation] = stamp
	u.SetAnnotations(ann)
	_, _ = dyn.Resource(gvr).Update(ctx, u, metav1.UpdateOptions{})
}
