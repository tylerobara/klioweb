package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type fakeExec struct {
	mu       sync.Mutex
	recovery string // answer for pg_is_in_recovery()
	calls    []string
}

func (f *fakeExec) Run(_ context.Context, ns, pod, container string, cmd []string) (string, error) {
	sql := cmd[len(cmd)-1]
	f.mu.Lock()
	f.calls = append(f.calls, ns+"/"+pod+"/"+container+": "+sql)
	f.mu.Unlock()
	switch {
	case sql == "select 1":
		return "1\n", nil
	case strings.Contains(sql, "pg_is_in_recovery"):
		return f.recovery + "\n", nil
	case strings.Contains(sql, "from pg_database"):
		return "app\n", nil
	case strings.Contains(sql, "pg_class"):
		return "43|11 MB\n", nil
	}
	return "", errors.New("unexpected sql: " + sql)
}

func obj(apiVersion, kind, ns, name string, spec, status map[string]any) *unstructured.Unstructured {
	o := map[string]any{"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns}}
	if spec != nil {
		o["spec"] = spec
	}
	if status != nil {
		o["status"] = status
	}
	return &unstructured.Unstructured{Object: o}
}

type env struct {
	m    *Manager
	dyn  *dynamicfake.FakeDynamicClient
	kube *k8sfake.Clientset
	exec *fakeExec
	made *unstructured.Unstructured // test cluster as created
	pc   *unstructured.Unstructured // test plugin configuration as created
}

func newEnv(t *testing.T, healthy bool, mut func(*Config)) *env {
	t.Helper()
	src := obj("postgresql.cnpg.io/v1", "Cluster", "default", "app", map[string]any{
		"instances": int64(3),
		"storage":   map[string]any{"size": "5Gi", "storageClass": "fast"},
		"postgresql": map[string]any{"parameters": map[string]any{"max_connections": "300"},
			"shared_preload_libraries": []any{"vchord.so"}},
		"plugins": []any{map[string]any{"name": "klio.cnpg.io", "enabled": true,
			"parameters": map[string]any{"pluginConfigurationRef": "cfg"}}},
	}, map[string]any{"image": "registry/pg:18.6"})
	other := obj("postgresql.cnpg.io/v1", "Cluster", "default", "plain", map[string]any{"instances": int64(1)}, nil)
	cfg := obj("klio.cnpg.io/v1alpha1", "PluginConfiguration", "default", "cfg",
		map[string]any{"clusterName": "app", "serverAddress": "klio.default"}, nil)
	good := obj("postgresql.cnpg.io/v1", "Backup", "default", "b1", map[string]any{
		"cluster": map[string]any{"name": "app"}, "method": "plugin",
		"pluginConfiguration": map[string]any{"name": "klio.cnpg.io"}},
		map[string]any{"phase": "completed", "backupId": "backup-1"})
	failed := obj("postgresql.cnpg.io/v1", "Backup", "default", "bad", map[string]any{
		"cluster": map[string]any{"name": "app"}, "method": "plugin",
		"pluginConfiguration": map[string]any{"name": "klio.cnpg.io"}},
		map[string]any{"phase": "failed"})
	foreign := obj("postgresql.cnpg.io/v1", "Backup", "default", "theirs", map[string]any{
		"cluster": map[string]any{"name": "plain"}, "method": "plugin",
		"pluginConfiguration": map[string]any{"name": "klio.cnpg.io"}},
		map[string]any{"phase": "completed", "backupId": "backup-9"})

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gvrClusters: "ClusterList", gvrBackups: "BackupList", gvrPlugCfg: "PluginConfigurationList"},
		src, other, cfg, good, failed, foreign)
	e := &env{dyn: dyn, kube: k8sfake.NewSimpleClientset(), exec: &fakeExec{recovery: "f"}}

	dyn.PrependReactor("create", "pluginconfigurations", func(a clienttesting.Action) (bool, runtime.Object, error) {
		e.pc = a.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		return false, nil, nil
	})
	dyn.PrependReactor("create", "clusters", func(a clienttesting.Action) (bool, runtime.Object, error) {
		o := a.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		e.made = o.DeepCopy()
		if healthy { // simulate CNPG finishing the restore
			unstructured.SetNestedField(o.Object, "Cluster in healthy state", "status", "phase")
			unstructured.SetNestedField(o.Object, int64(1), "status", "readyInstances")
			unstructured.SetNestedField(o.Object, o.GetName()+"-1", "status", "currentPrimary")
		}
		return false, nil, nil
	})

	c := Config{Namespaces: []string{"default"}, OwnNamespace: "klioweb", Timeout: 2 * time.Second,
		PollInterval: 10 * time.Millisecond, MaxConcurrent: 1}
	if mut != nil {
		mut(&c)
	}
	e.m = New(context.Background(), c, dyn, e.kube, e.exec)
	return e
}

func (e *env) await(t *testing.T, id string) Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := e.m.Get(context.Background(), id)
		if err == nil && r.Terminal() {
			return *r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return Run{}
}

func (e *env) exists(res schema.GroupVersionResource, name string) bool {
	_, err := e.dyn.Resource(res).Namespace("default").Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func TestHappyPath(t *testing.T) {
	e := newEnv(t, true, nil)
	run, err := e.m.Trigger(context.Background(), "tyler", "default", "app", "b1")
	if err != nil {
		t.Fatal(err)
	}
	r := e.await(t, run.ID)
	if r.Status != StatusPassed || r.Error != "" {
		t.Fatalf("status=%s err=%q checks=%+v", r.Status, r.Error, r.Checks)
	}
	if r.TriggeredBy != "tyler" || r.BackupID != "backup-1" || r.FinishedAt == nil {
		t.Fatalf("bad run: %+v", r)
	}
	names := map[string]bool{}
	for _, c := range r.Checks {
		if !c.OK {
			t.Errorf("check failed: %+v", c)
		}
		names[c.Name] = true
	}
	for _, want := range []string{"restore completes", "accepts connections", "promoted out of recovery", "database app"} {
		if !names[want] {
			t.Errorf("missing check %q in %+v", want, r.Checks)
		}
	}
	// teardown
	if e.exists(gvrClusters, r.TestCluster) || e.exists(gvrPlugCfg, r.TestCluster) {
		t.Fatal("test objects were not removed")
	}
	if r.CleanupError != "" {
		t.Fatalf("cleanup error: %s", r.CleanupError)
	}
	// the source cluster is untouched
	if !e.exists(gvrClusters, "app") {
		t.Fatal("source cluster disappeared")
	}
	// the restore cluster was cloned correctly
	spec := e.made.Object["spec"].(map[string]any)
	if got, _, _ := unstructured.NestedString(spec, "bootstrap", "recovery", "recoveryTarget", "backupID"); got != "backup-1" {
		t.Errorf("backupID = %q", got)
	}
	if spec["imageName"] != "registry/pg:18.6" || spec["instances"] != int64(1) {
		t.Errorf("image/instances wrong: %v %v", spec["imageName"], spec["instances"])
	}
	if sz, _, _ := unstructured.NestedString(spec, "storage", "size"); sz != "5Gi" {
		t.Errorf("storage size = %q", sz)
	}
	if sc, _, _ := unstructured.NestedString(spec, "storage", "storageClass"); sc != "fast" {
		t.Errorf("storage class = %q", sc)
	}
	if mc, _, _ := unstructured.NestedString(spec, "postgresql", "parameters", "max_connections"); mc != "300" {
		t.Errorf("postgres parameters not copied: %v", spec["postgresql"])
	}
	if _, has := spec["plugins"]; has {
		t.Error("restore cluster must not enable the klio plugin (it would take backups)")
	}
	if e.made.GetLabels()[LabelTest] != "true" || e.pc.GetLabels()[LabelTest] != "true" {
		t.Error("created objects must be labelled")
	}
	if cn, _, _ := unstructured.NestedString(e.pc.Object, "spec", "clusterName"); cn != "app" {
		t.Errorf("plugin config clusterName = %q, want the original", cn)
	}
	ref, _, _ := unstructured.NestedString(spec, "externalClusters", "0", "plugin")
	_ = ref
	ext := spec["externalClusters"].([]any)[0].(map[string]any)["plugin"].(map[string]any)["parameters"].(map[string]any)
	if ext["pluginConfigurationRef"] != r.TestCluster {
		t.Errorf("externalClusters ref = %v", ext["pluginConfigurationRef"])
	}
	// SQL ran in the restored pod's postgres container
	if len(e.exec.calls) == 0 || !strings.HasPrefix(e.exec.calls[0], "default/"+r.TestCluster+"-1/postgres:") {
		t.Errorf("exec calls: %v", e.exec.calls)
	}
	// a result ConfigMap was stored
	if _, err := e.kube.CoreV1().ConfigMaps("klioweb").Get(context.Background(), cmName(r.ID), metav1.GetOptions{}); err != nil {
		t.Errorf("no stored result: %v", err)
	}
}

func TestFailingCheckStillTearsDown(t *testing.T) {
	e := newEnv(t, true, nil)
	e.exec.recovery = "t"
	run, _ := e.m.Trigger(context.Background(), "u", "default", "app", "b1")
	r := e.await(t, run.ID)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "promoted out of recovery") {
		t.Fatalf("status=%s err=%q", r.Status, r.Error)
	}
	if e.exists(gvrClusters, r.TestCluster) || e.exists(gvrPlugCfg, r.TestCluster) {
		t.Fatal("objects left behind after a failed run")
	}
}

func TestTimeoutFailsAndTearsDown(t *testing.T) {
	e := newEnv(t, false, func(c *Config) { c.Timeout = 150 * time.Millisecond })
	e.kube.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.EventList{Items: []corev1.Event{{
			InvolvedObject: corev1.ObjectReference{Name: "x"}, Type: "Warning", Reason: "R", Message: "boom"}}}, nil
	})
	run, _ := e.m.Trigger(context.Background(), "u", "default", "app", "b1")
	r := e.await(t, run.ID)
	if r.Status != StatusFailed || !strings.Contains(r.Error, "timed out") {
		t.Fatalf("status=%s err=%q", r.Status, r.Error)
	}
	if e.exists(gvrClusters, r.TestCluster) || e.exists(gvrPlugCfg, r.TestCluster) {
		t.Fatal("objects left behind after timeout")
	}
	if len(e.exec.calls) != 0 {
		t.Fatal("must not run SQL when the restore never completed")
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, true, nil)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		ns, cluster, backup string
		want                error
	}{
		"namespace not allowed": {"kube-system", "app", "b1", ErrForbidden},
		"missing fields":        {"default", "", "b1", ErrInvalid},
		"unknown cluster":       {"default", "nope", "b1", ErrInvalid},
		"cluster without klio":  {"default", "plain", "theirs", ErrInvalid},
		"unknown backup":        {"default", "app", "nope", ErrInvalid},
		"failed backup":         {"default", "app", "bad", ErrInvalid},
		"backup of other db":    {"default", "app", "theirs", ErrInvalid},
	} {
		if _, err := e.m.Trigger(ctx, "u", tc.ns, tc.cluster, tc.backup); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if e.made != nil {
		t.Fatal("a rejected request created resources")
	}
}

func TestBusy(t *testing.T) {
	e := newEnv(t, false, func(c *Config) { c.Timeout = 5 * time.Second })
	first, err := e.m.Trigger(context.Background(), "u", "default", "app", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Trigger(context.Background(), "u", "default", "app", "b1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second run for the same cluster: %v", err)
	}
	_ = first
}

func TestRefusesToDeleteUnlabelled(t *testing.T) {
	e := newEnv(t, true, nil)
	err := e.m.deleteOwned(context.Background(), gvrClusters, "default", "app")
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if !e.exists(gvrClusters, "app") {
		t.Fatal("unlabelled cluster was deleted")
	}
}

func TestRecover(t *testing.T) {
	e := newEnv(t, true, nil)
	ctx := context.Background()
	orphan := obj("postgresql.cnpg.io/v1", "Cluster", "default", "app-rt-aaaaaa", map[string]any{}, nil)
	orphan.SetLabels(map[string]string{LabelTest: "true"})
	e.dyn.Resource(gvrClusters).Namespace("default").Create(ctx, orphan, metav1.CreateOptions{})
	stuck := Run{ID: "stuck-1", Namespace: "default", Cluster: "app", Status: StatusRestoring, CreatedAt: time.Now()}
	e.m.store.save(ctx, stuck)

	e.m.Recover(ctx)

	if e.exists(gvrClusters, "app-rt-aaaaaa") {
		t.Error("orphaned restore cluster not removed")
	}
	if !e.exists(gvrClusters, "app") || !e.exists(gvrClusters, "plain") {
		t.Error("recover removed a real cluster")
	}
	r, _ := e.m.Get(ctx, "stuck-1")
	if r.Status != StatusFailed || !strings.Contains(r.Error, "interrupted") {
		t.Errorf("stuck run: %+v", r)
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	e := newEnv(t, true, func(c *Config) { c.Keep = 2 })
	ctx := context.Background()
	for i, id := range []string{"a", "b", "c", "d"} {
		e.m.store.save(ctx, Run{ID: id, Namespace: "default", Cluster: "app", Status: StatusPassed,
			CreatedAt: time.Now().Add(time.Duration(i) * time.Minute)})
	}
	e.m.store.prune(ctx, 2)
	runs, _ := e.m.List(ctx)
	if len(runs) != 2 || runs[0].ID != "d" || runs[1].ID != "c" {
		t.Fatalf("kept %+v", runs)
	}
}

type fakeGuard struct{ loggedIn bool }

func (g fakeGuard) RequireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.loggedIn {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (g fakeGuard) UserName(*http.Request) (string, bool) { return "tyler", g.loggedIn }

func TestHTTP(t *testing.T) {
	post := func(h http.Handler, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/restore-tests", bytes.NewBufferString(body)))
		return rr
	}
	e := newEnv(t, true, nil)

	anon := http.NewServeMux()
	e.m.Register(anon, fakeGuard{loggedIn: false})
	if rr := post(anon, `{"namespace":"default","cluster":"app","backup":"b1"}`); rr.Code != 401 {
		t.Fatalf("anonymous POST = %d, want 401", rr.Code)
	}
	if e.made != nil {
		t.Fatal("anonymous request created resources")
	}
	rr := httptest.NewRecorder()
	anon.ServeHTTP(rr, httptest.NewRequest("GET", "/api/restore-tests", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("anonymous GET = %d %s", rr.Code, rr.Body)
	}

	authed := http.NewServeMux()
	e.m.Register(authed, fakeGuard{loggedIn: true})
	if rr := post(authed, `not json`); rr.Code != 400 {
		t.Errorf("bad json = %d", rr.Code)
	}
	if rr := post(authed, `{"namespace":"kube-system","cluster":"app","backup":"b1"}`); rr.Code != 403 {
		t.Errorf("forbidden ns = %d", rr.Code)
	}
	if rr := post(authed, `{"namespace":"default","cluster":"app","backup":"nope"}`); rr.Code != 400 {
		t.Errorf("unknown backup = %d", rr.Code)
	}
	rr = post(authed, `{"namespace":"default","cluster":"app","backup":"b1"}`)
	if rr.Code != 202 {
		t.Fatalf("POST = %d %s", rr.Code, rr.Body)
	}
	var run Run
	json.Unmarshal(rr.Body.Bytes(), &run)
	e.await(t, run.ID)
	rr = httptest.NewRecorder()
	authed.ServeHTTP(rr, httptest.NewRequest("GET", "/api/restore-tests/"+run.ID, nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"passed"`) {
		t.Fatalf("GET run = %d %s", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	authed.ServeHTTP(rr, httptest.NewRequest("GET", "/api/restore-tests/missing", nil))
	if rr.Code != 404 {
		t.Errorf("missing run = %d", rr.Code)
	}
}
