package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var (
	gvrClusters = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	gvrBackups  = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
	gvrPlugCfg  = schema.GroupVersionResource{Group: "klio.cnpg.io", Version: "v1alpha1", Resource: "pluginconfigurations"}
)

const (
	klioPlugin     = "klio.cnpg.io"
	healthyPhase   = "Cluster in healthy state"
	pgContainer    = "postgres"
	maxClusterName = 36 // keeps <name>-rt-xxxxxx-1 and service names well under 63 chars
)

type Config struct {
	Namespaces    []string      // namespaces where tests may run
	OwnNamespace  string        // where result ConfigMaps live
	Timeout       time.Duration // per run, restore phase
	Keep          int           // finished runs kept per source cluster
	MaxConcurrent int
	PollInterval  time.Duration
}

type Manager struct {
	cfg   Config
	dyn   dynamic.Interface
	kube  kubernetes.Interface
	exec  Exec
	store *store
	base  context.Context

	mu     sync.Mutex
	active map[string]string // source ns/cluster -> run id
}

func New(base context.Context, cfg Config, dyn dynamic.Interface, kube kubernetes.Interface, exec Exec) *Manager {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 20
	}
	return &Manager{cfg: cfg, dyn: dyn, kube: kube, exec: exec, base: base,
		store: &store{kube: kube, ns: cfg.OwnNamespace}, active: map[string]string{}}
}

func (m *Manager) Namespaces() []string { return m.cfg.Namespaces }

func (m *Manager) allowed(ns string) bool { return slices.Contains(m.cfg.Namespaces, ns) }

// Recover runs at startup: runs left unfinished by a previous process are
// marked failed and any test objects they left behind are deleted.
func (m *Manager) Recover(ctx context.Context) {
	runs, err := m.store.list(ctx)
	if err != nil {
		log.Printf("restore: recover: %v", err)
	}
	for _, r := range runs {
		if r.Terminal() {
			continue
		}
		r.Status, r.Error = StatusFailed, "interrupted: klioweb restarted during the run"
		now := time.Now().UTC()
		r.FinishedAt = &now
		m.store.save(ctx, r)
	}
	for _, ns := range m.cfg.Namespaces {
		for _, res := range []schema.GroupVersionResource{gvrClusters, gvrPlugCfg} {
			l, err := m.dyn.Resource(res).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelTest + "=true"})
			if err != nil {
				log.Printf("restore: recover: list %s in %s: %v", res.Resource, ns, err)
				continue
			}
			for _, o := range l.Items {
				log.Printf("restore: removing orphaned %s %s/%s", res.Resource, ns, o.GetName())
				m.deleteOwned(ctx, res, ns, o.GetName())
			}
		}
	}
	m.store.prune(ctx, m.cfg.Keep)
}

func (m *Manager) List(ctx context.Context) ([]Run, error) { return m.store.list(ctx) }
func (m *Manager) Get(ctx context.Context, id string) (*Run, error) {
	return m.store.get(ctx, id)
}

// Trigger validates the request, records a pending run and starts it.
func (m *Manager) Trigger(ctx context.Context, user, ns, cluster, backup string) (*Run, error) {
	if ns == "" || cluster == "" || backup == "" {
		return nil, fmt.Errorf("%w: namespace, cluster and backup are required", ErrInvalid)
	}
	if !m.allowed(ns) {
		return nil, ErrForbidden
	}
	src, err := m.dyn.Resource(gvrClusters).Namespace(ns).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: cluster %s/%s: %v", ErrInvalid, ns, cluster, err)
	}
	if _, ok := klioConfigRef(src); !ok {
		return nil, fmt.Errorf("%w: cluster %s/%s does not use the klio plugin", ErrInvalid, ns, cluster)
	}
	bk, err := m.dyn.Resource(gvrBackups).Namespace(ns).Get(ctx, backup, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: backup %s: %v", ErrInvalid, backup, err)
	}
	bcluster, _, _ := unstructured.NestedString(bk.Object, "spec", "cluster", "name")
	bplugin, _, _ := unstructured.NestedString(bk.Object, "spec", "pluginConfiguration", "name")
	phase, _, _ := unstructured.NestedString(bk.Object, "status", "phase")
	backupID, _, _ := unstructured.NestedString(bk.Object, "status", "backupId")
	switch {
	case bcluster != cluster || bplugin != klioPlugin:
		return nil, fmt.Errorf("%w: backup %s does not belong to cluster %s", ErrInvalid, backup, cluster)
	case phase != "completed" || backupID == "":
		return nil, fmt.Errorf("%w: backup %s is not a completed backup", ErrInvalid, backup)
	}

	key := ns + "/" + cluster
	id := newID()
	m.mu.Lock()
	if other, busy := m.active[key]; busy {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: a test of %s is already running (%s)", ErrBusy, key, other)
	}
	if len(m.active) >= m.cfg.MaxConcurrent {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.active[key] = id
	m.mu.Unlock()

	run := Run{
		ID: id, Namespace: ns, Cluster: cluster, Backup: backup, BackupID: backupID,
		TestCluster: testName(cluster, id), TriggeredBy: user, Status: StatusPending,
		CreatedAt: time.Now().UTC(), Checks: []Check{},
	}
	if err := m.store.save(ctx, run); err != nil {
		m.release(key)
		return nil, fmt.Errorf("saving run: %w", err)
	}
	// The goroutine mutates its own copy; the caller gets an untouched snapshot.
	work := run
	go func() {
		defer m.release(key)
		m.execute(m.base, &work)
	}()
	return &run, nil
}

func (m *Manager) release(key string) {
	m.mu.Lock()
	delete(m.active, key)
	m.mu.Unlock()
}

func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return time.Now().UTC().Format("20060102t150405") + "-" + hex.EncodeToString(b)
}

func testName(cluster, id string) string {
	if len(cluster) > maxClusterName {
		cluster = strings.TrimRight(cluster[:maxClusterName], "-")
	}
	return cluster + "-rt-" + id[len(id)-6:]
}

func klioConfigRef(cl *unstructured.Unstructured) (string, bool) {
	plugins, _, _ := unstructured.NestedSlice(cl.Object, "spec", "plugins")
	for _, p := range plugins {
		pm, _ := p.(map[string]any)
		if pm["name"] != klioPlugin {
			continue
		}
		if en, ok := pm["enabled"].(bool); ok && !en {
			return "", false
		}
		params, _ := pm["parameters"].(map[string]any)
		ref, _ := params["pluginConfigurationRef"].(string)
		return ref, ref != ""
	}
	return "", false
}

// execute performs the whole run. Teardown always happens.
func (m *Manager) execute(ctx context.Context, run *Run) {
	started := time.Now()
	set := func(status string) {
		run.Status = status
		if err := m.store.save(ctx, *run); err != nil {
			log.Printf("restore %s: save: %v", run.ID, err)
		}
	}
	fail := func(format string, a ...any) {
		run.Error = fmt.Sprintf(format, a...)
	}

	set(StatusRestoring)
	created := false
	func() {
		pc, cl, err := m.buildObjects(ctx, run)
		if err != nil {
			fail("preparing restore: %v", err)
			return
		}
		created = true // from here on, teardown has work to do
		if _, err := m.dyn.Resource(gvrPlugCfg).Namespace(run.Namespace).Create(ctx, pc, metav1.CreateOptions{}); err != nil {
			fail("creating PluginConfiguration: %v", err)
			return
		}
		if _, err := m.dyn.Resource(gvrClusters).Namespace(run.Namespace).Create(ctx, cl, metav1.CreateOptions{}); err != nil {
			fail("creating restore cluster: %v", err)
			return
		}
		restoreStart := time.Now()
		primary, err := m.waitHealthy(ctx, run)
		if err != nil {
			fail("%v", err)
			run.Diagnostics = m.diagnostics(ctx, run)
			run.Checks = append(run.Checks, Check{Name: "restore completes", OK: false, Detail: err.Error()})
			return
		}
		run.Checks = append(run.Checks, Check{Name: "restore completes", OK: true,
			Detail: "cluster healthy after " + time.Since(restoreStart).Round(time.Second).String()})
		set(StatusChecking)
		m.runChecks(ctx, run, primary)
	}()

	for _, c := range run.Checks {
		if !c.OK && run.Error == "" {
			run.Error = "check failed: " + c.Name
		}
	}

	set(StatusCleaning)
	if created {
		if err := m.teardown(run); err != nil {
			run.CleanupError = err.Error()
			log.Printf("restore %s: cleanup: %v", run.ID, err)
		}
	}
	now := time.Now().UTC()
	run.FinishedAt = &now
	run.DurationMs = time.Since(started).Milliseconds()
	run.Status = StatusPassed
	if run.Error != "" {
		run.Status = StatusFailed
	}
	if err := m.store.save(context.WithoutCancel(ctx), *run); err != nil {
		log.Printf("restore %s: final save: %v", run.ID, err)
	}
	m.store.prune(context.WithoutCancel(ctx), m.cfg.Keep)
	log.Printf("restore %s: %s/%s backup %s -> %s (%s)", run.ID, run.Namespace, run.Cluster, run.Backup, run.Status,
		time.Since(started).Round(time.Second))
}

// buildObjects clones the source cluster's recovery-relevant settings.
func (m *Manager) buildObjects(ctx context.Context, run *Run) (pc, cl *unstructured.Unstructured, err error) {
	src, err := m.dyn.Resource(gvrClusters).Namespace(run.Namespace).Get(ctx, run.Cluster, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	ref, ok := klioConfigRef(src)
	if !ok {
		return nil, nil, fmt.Errorf("cluster no longer uses the klio plugin")
	}
	srcCfg, err := m.dyn.Resource(gvrPlugCfg).Namespace(run.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("plugin configuration %q: %w", ref, err)
	}
	return buildPluginConfig(srcCfg, run), buildCluster(src, run), nil
}

func labels(run *Run) map[string]any {
	return map[string]any{LabelTest: "true", LabelRun: run.ID}
}

func buildPluginConfig(src *unstructured.Unstructured, run *Run) *unstructured.Unstructured {
	spec, _, _ := unstructured.NestedMap(src.Object, "spec")
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "klio.cnpg.io/v1alpha1", "kind": "PluginConfiguration",
		"metadata": map[string]any{"name": run.TestCluster, "namespace": run.Namespace, "labels": labels(run)},
		"spec":     spec, // clusterName stays the original: Klio identifies backups by it
	}}
}

func buildCluster(src *unstructured.Unstructured, run *Run) *unstructured.Unstructured {
	spec := map[string]any{
		"instances": int64(1),
		// Requests only: enough for the scheduler, no limit to OOM-kill a replay.
		"resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "256Mi"}},
		"bootstrap": map[string]any{"recovery": map[string]any{
			"source":         "source",
			"recoveryTarget": map[string]any{"backupID": run.BackupID},
		}},
		"externalClusters": []any{map[string]any{
			"name": "source",
			"plugin": map[string]any{"name": klioPlugin,
				"parameters": map[string]any{"pluginConfigurationRef": run.TestCluster}},
		}},
	}
	// The recovered instance must use the same image and at least the source's
	// Postgres settings (max_connections etc. are checked against the backup).
	if img, _, _ := unstructured.NestedString(src.Object, "status", "image"); img != "" {
		spec["imageName"] = img
	} else if img, _, _ := unstructured.NestedString(src.Object, "spec", "imageName"); img != "" {
		spec["imageName"] = img
	}
	if ref, ok, _ := unstructured.NestedMap(src.Object, "spec", "imageCatalogRef"); ok {
		spec["imageCatalogRef"] = ref
		delete(spec, "imageName")
	}
	if pg, ok, _ := unstructured.NestedMap(src.Object, "spec", "postgresql"); ok {
		spec["postgresql"] = pg
	}
	storage := map[string]any{}
	for _, k := range []string{"size", "storageClass"} {
		if v, ok, _ := unstructured.NestedString(src.Object, "spec", "storage", k); ok && v != "" {
			storage[k] = v
		}
	}
	spec["storage"] = storage
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": run.TestCluster, "namespace": run.Namespace, "labels": labels(run)},
		"spec":     spec,
	}}
}

// waitHealthy polls until the restore cluster is healthy, then returns its primary pod.
func (m *Manager) waitHealthy(ctx context.Context, run *Run) (string, error) {
	deadline := time.Now().Add(m.cfg.Timeout)
	var phase string
	for {
		cl, err := m.dyn.Resource(gvrClusters).Namespace(run.Namespace).Get(ctx, run.TestCluster, metav1.GetOptions{})
		if err == nil {
			phase, _, _ = unstructured.NestedString(cl.Object, "status", "phase")
			ready, _, _ := unstructured.NestedInt64(cl.Object, "status", "readyInstances")
			if phase == healthyPhase && ready >= 1 {
				primary, _, _ := unstructured.NestedString(cl.Object, "status", "currentPrimary")
				if primary == "" {
					primary = run.TestCluster + "-1"
				}
				return primary, nil
			}
		}
		if n := m.failedRecoveryPods(ctx, run); n >= 2 {
			return "", fmt.Errorf("recovery job failed %d times (cluster phase %q)", n, phase)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for the restore (last phase %q)", m.cfg.Timeout, phase)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.cfg.PollInterval):
		}
	}
}

// teardown deletes the test objects (never anything without our label) and
// waits for the data volume to go away.
func (m *Manager) teardown(run *Run) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var errs []string
	if err := m.deleteOwned(ctx, gvrClusters, run.Namespace, run.TestCluster); err != nil {
		errs = append(errs, err.Error())
	}
	if err := m.deleteOwned(ctx, gvrPlugCfg, run.Namespace, run.TestCluster); err != nil {
		errs = append(errs, err.Error())
	}
	for {
		l, err := m.kube.CoreV1().PersistentVolumeClaims(run.Namespace).List(ctx,
			metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + run.TestCluster})
		if err == nil && len(l.Items) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			errs = append(errs, "timed out waiting for the restore volume to be removed")
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		case <-time.After(m.cfg.PollInterval):
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// deleteOwned deletes the object only if it carries our label.
func (m *Manager) deleteOwned(ctx context.Context, res schema.GroupVersionResource, ns, name string) error {
	o, err := m.dyn.Resource(res).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if o.GetLabels()[LabelTest] != "true" {
		return fmt.Errorf("refusing to delete %s %s/%s: not created by a restore test", res.Resource, ns, name)
	}
	err = m.dyn.Resource(res).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
