package backup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func cluster(ns, name string, klio bool) *unstructured.Unstructured {
	spec := map[string]any{}
	if klio {
		spec["plugins"] = []any{map[string]any{"name": klioPlugin}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": name, "namespace": ns}, "spec": spec}}
}

func bk(ns, name, cluster, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"cluster": map[string]any{"name": cluster}},
		"status":   map[string]any{"phase": phase}}}
}

func newMgr(objs ...runtime.Object) *Manager {
	s := runtime.NewScheme()
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(s, map[schema.GroupVersionResource]string{
		gvrClusters: "ClusterList", gvrBackups: "BackupList"}, objs...)
	return New([]string{"default"}, d)
}

func TestTrigger(t *testing.T) {
	ctx := context.Background()
	m := newMgr(cluster("default", "pg", true), cluster("default", "plain", false),
		cluster("other", "pg", true), bk("default", "old", "pg", "completed"))
	if _, err := m.Trigger(ctx, "u", "other", "pg"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want forbidden, got %v", err)
	}
	if _, err := m.Trigger(ctx, "u", "default", "plain"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
	if _, err := m.Trigger(ctx, "u", "default", "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
	name, err := m.Trigger(ctx, "alice", "default", "pg")
	if err != nil || !strings.HasPrefix(name, "pg-klio-ui-") {
		t.Fatalf("trigger: %q %v", name, err)
	}
	got, err := m.dyn.Resource(gvrBackups).Namespace("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := unstructured.NestedString(got.Object, "spec", "pluginConfiguration", "name"); v != klioPlugin {
		t.Fatalf("plugin %q", v)
	}
	if got.GetAnnotations()[AnnoUser] != "alice" || got.GetLabels()[LabelManual] != "true" {
		t.Fatalf("metadata: %v %v", got.GetAnnotations(), got.GetLabels())
	}
	// The new backup has no phase yet, so a second request is refused.
	if _, err := m.Trigger(ctx, "alice", "default", "pg"); !errors.Is(err, ErrBusy) {
		t.Fatalf("want busy, got %v", err)
	}
}

type guard struct{ ok bool }

func (g guard) RequireLogin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.ok {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
func (g guard) UserName(*http.Request) (string, bool) { return "bob", g.ok }

func TestHTTP(t *testing.T) {
	post := func(g guard, body string) int {
		m := newMgr(cluster("default", "pg", true))
		mux := http.NewServeMux()
		m.Register(mux, g)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/backups", strings.NewReader(body)))
		return rec.Code
	}
	if c := post(guard{false}, `{}`); c != 401 {
		t.Fatalf("anon: %d", c)
	}
	if c := post(guard{true}, `{"namespace":"default","cluster":"pg","x":1}`); c != 400 {
		t.Fatalf("unknown field: %d", c)
	}
	if c := post(guard{true}, `{"namespace":"x","cluster":"pg"}`); c != 403 {
		t.Fatalf("forbidden: %d", c)
	}
	if c := post(guard{true}, `{"namespace":"default","cluster":"pg"}`); c != 202 {
		t.Fatalf("ok: %d", c)
	}
}
