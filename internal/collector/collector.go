// Package collector reads CloudNativePG and Klio resources from the cluster
// and condenses them into a Snapshot for the dashboard.
package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	klioPluginName = "klio.cnpg.io"
	recentBackups  = 15
	historyLen     = 40
	defaultEvery   = 24 * time.Hour
)

var (
	gvrClusters  = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	gvrBackups   = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
	gvrSchedules = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "scheduledbackups"}
	gvrPlugCfg   = schema.GroupVersionResource{Group: "klio.cnpg.io", Version: "v1alpha1", Resource: "pluginconfigurations"}
	gvrServers   = schema.GroupVersionResource{Group: "klio.cnpg.io", Version: "v1alpha1", Resource: "servers"}
	gvrPods      = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

type Collector struct {
	client dynamic.Interface
	ttl    time.Duration

	mu       sync.Mutex
	cached   *Snapshot
	cachedAt time.Time
	lastErr  error
}

func New(client dynamic.Interface, ttl time.Duration) *Collector {
	return &Collector{client: client, ttl: ttl}
}

// Snapshot returns a recent snapshot, refreshing it when older than the TTL.
func (c *Collector) Snapshot(ctx context.Context) (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && time.Since(c.cachedAt) < c.ttl {
		return c.cached, nil
	}
	s, err := c.collect(ctx)
	if err != nil {
		if c.cached != nil { // serve stale data rather than failing the UI
			return c.cached, nil
		}
		return nil, err
	}
	c.cached, c.cachedAt = s, time.Now()
	return s, nil
}

func (c *Collector) list(ctx context.Context, gvr schema.GroupVersionResource, optional bool) ([]unstructured.Unstructured, error) {
	l, err := c.client.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		if optional {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
	}
	return l.Items, nil
}

func (c *Collector) collect(ctx context.Context) (*Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	clusters, err := c.list(ctx, gvrClusters, false)
	if err != nil {
		return nil, err
	}
	backups, err := c.list(ctx, gvrBackups, false)
	if err != nil {
		return nil, err
	}
	schedules, err := c.list(ctx, gvrSchedules, false)
	if err != nil {
		return nil, err
	}
	// Klio CRDs may be absent if Klio isn't installed; tolerate that.
	plugCfgs, _ := c.list(ctx, gvrPlugCfg, true)
	servers, _ := c.list(ctx, gvrServers, true)
	pods, _ := c.list(ctx, gvrPods, true)

	cfgIdx := map[string]*unstructured.Unstructured{}
	for i := range plugCfgs {
		cfgIdx[key(plugCfgs[i].GetNamespace(), plugCfgs[i].GetName())] = &plugCfgs[i]
	}

	// Group klio plugin backups by cluster.
	bkByCluster := map[string][]unstructured.Unstructured{}
	for _, b := range backups {
		if !isKlioBackup(b) {
			continue
		}
		cn, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name")
		k := key(b.GetNamespace(), cn)
		bkByCluster[k] = append(bkByCluster[k], b)
	}
	schedByCluster := map[string][]unstructured.Unstructured{}
	for _, s := range schedules {
		if pn, _, _ := unstructured.NestedString(s.Object, "spec", "pluginConfiguration", "name"); pn != klioPluginName {
			continue
		}
		cn, _, _ := unstructured.NestedString(s.Object, "spec", "cluster", "name")
		k := key(s.GetNamespace(), cn)
		schedByCluster[k] = append(schedByCluster[k], s)
	}

	// Servers and their pod readiness.
	srvIdx := map[string]*ServerStatus{}
	var serverList []ServerStatus
	for _, s := range servers {
		ss := buildServer(s, pods)
		serverList = append(serverList, ss)
	}
	for i := range serverList {
		srvIdx[key(serverList[i].Namespace, serverList[i].Name)] = &serverList[i]
	}

	snap := &Snapshot{GeneratedAt: time.Now().UTC()}
	now := time.Now()
	for _, cl := range clusters {
		if cl.GetLabels()["klioweb.obara.dev/restore-test"] == "true" {
			continue // short-lived restore verification clusters are not real databases
		}
		cs := buildCluster(cl)
		k := key(cs.Namespace, cs.Name)
		if cs.KlioEnabled {
			cs.Klio = buildKlio(cl, cfgIdx, srvIdx, bkByCluster[k], schedByCluster[k], now)
			if cs.Klio.ServerName != "" {
				if sv := srvIdx[key(serverNS(cs.Klio, cs.Namespace), cs.Klio.ServerName)]; sv != nil {
					sv.Clusters = append(sv.Clusters, cs.Namespace+"/"+cs.Name)
				}
			}
		}
		snap.Clusters = append(snap.Clusters, cs)
	}
	sort.Slice(snap.Clusters, func(i, j int) bool {
		a, b := snap.Clusters[i], snap.Clusters[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	sort.Slice(serverList, func(i, j int) bool {
		if serverList[i].Namespace != serverList[j].Namespace {
			return serverList[i].Namespace < serverList[j].Namespace
		}
		return serverList[i].Name < serverList[j].Name
	})
	for i := range serverList {
		sort.Strings(serverList[i].Clusters)
	}
	snap.Servers = serverList
	snap.Summary = summarize(snap)
	return snap, nil
}

// rank orders clusters: critical first, then warning, ok, then klio-less.
func rank(c ClusterStatus) int {
	if c.Klio == nil {
		return 4
	}
	switch c.Klio.Health {
	case HealthCritical:
		return 0
	case HealthWarning:
		return 1
	}
	return 2
}

func summarize(s *Snapshot) Summary {
	sum := Summary{TotalClusters: len(s.Clusters), Servers: len(s.Servers)}
	for _, c := range s.Clusters {
		if c.Klio == nil {
			sum.KlioDisabled++
			continue
		}
		sum.KlioEnabled++
		switch c.Klio.Health {
		case HealthOK:
			sum.Healthy++
		case HealthWarning:
			sum.Warning++
		case HealthCritical:
			sum.Critical++
		}
	}
	for _, sv := range s.Servers {
		if !sv.Ready {
			sum.ServersNotRead++
		}
	}
	return sum
}

func key(ns, name string) string { return ns + "/" + name }

func isKlioBackup(b unstructured.Unstructured) bool {
	if m, _, _ := unstructured.NestedString(b.Object, "spec", "method"); m != "plugin" {
		return false
	}
	pn, _, _ := unstructured.NestedString(b.Object, "spec", "pluginConfiguration", "name")
	return pn == klioPluginName
}

func buildCluster(cl unstructured.Unstructured) ClusterStatus {
	cs := ClusterStatus{Namespace: cl.GetNamespace(), Name: cl.GetName()}
	cs.Phase, _, _ = unstructured.NestedString(cl.Object, "status", "phase")
	cs.Instances, _, _ = unstructured.NestedInt64(cl.Object, "status", "instances")
	cs.ReadyInstances, _, _ = unstructured.NestedInt64(cl.Object, "status", "readyInstances")
	cs.Primary, _, _ = unstructured.NestedString(cl.Object, "status", "currentPrimary")
	cs.ImageName, _, _ = unstructured.NestedString(cl.Object, "status", "image")
	if cs.ImageName == "" {
		cs.ImageName, _, _ = unstructured.NestedString(cl.Object, "spec", "imageName")
	}
	_, ok := klioPlugin(cl)
	cs.KlioEnabled = ok
	return cs
}

// klioPlugin returns the klio entry of spec.plugins, if present and enabled.
func klioPlugin(cl unstructured.Unstructured) (map[string]any, bool) {
	plugins, _, _ := unstructured.NestedSlice(cl.Object, "spec", "plugins")
	for _, p := range plugins {
		m, ok := p.(map[string]any)
		if !ok || m["name"] != klioPluginName {
			continue
		}
		if en, ok := m["enabled"].(bool); ok && !en {
			return m, false
		}
		return m, true
	}
	return nil, false
}

func buildKlio(cl unstructured.Unstructured, cfgs map[string]*unstructured.Unstructured,
	servers map[string]*ServerStatus, backups, schedules []unstructured.Unstructured, now time.Time) *Klio {

	k := &Klio{Health: HealthOK, Reasons: []string{}, Recent: []Backup{}, History: []string{}}
	ns := cl.GetNamespace()
	plug, _ := klioPlugin(cl)
	k.IsWALArchiver, _ = plug["isWALArchiver"].(bool)
	if params, ok := plug["parameters"].(map[string]any); ok {
		k.PluginConfig, _ = params["pluginConfigurationRef"].(string)
	}

	warn := func(msg string) {
		if k.Health == HealthOK {
			k.Health = HealthWarning
		}
		k.Reasons = append(k.Reasons, msg)
	}
	crit := func(msg string) {
		k.Health = HealthCritical
		k.Reasons = append(k.Reasons, msg)
	}

	// --- plugin configuration + server ---
	if k.PluginConfig == "" {
		warn("Cluster has no pluginConfigurationRef")
	} else if cfg := cfgs[key(ns, k.PluginConfig)]; cfg == nil {
		crit(fmt.Sprintf("PluginConfiguration %q not found", k.PluginConfig))
	} else {
		k.ServerAddress, _, _ = unstructured.NestedString(cfg.Object, "spec", "serverAddress")
		k.Mode, _, _ = unstructured.NestedString(cfg.Object, "spec", "mode")
		k.Tier2Backup, _, _ = unstructured.NestedBool(cfg.Object, "spec", "tier2", "enableBackup")
		k.Tier2Recovery, _, _ = unstructured.NestedBool(cfg.Object, "spec", "tier2", "enableRecovery")
		conds, _, _ := unstructured.NestedSlice(cfg.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "ConfigurationApplied" {
				ok := m["status"] == "True"
				k.ConfigApplied = &ok
				k.ConfigMessage, _ = m["message"].(string)
			}
		}
		if k.ConfigApplied != nil && !*k.ConfigApplied {
			crit("PluginConfiguration not applied: " + k.ConfigMessage)
		}
		sn, sns := parseAddress(k.ServerAddress, ns)
		k.ServerName = sn
		if sns != ns {
			k.ServerName = sn
		}
		if sv := servers[key(sns, sn)]; sv == nil {
			warn(fmt.Sprintf("Klio server %s/%s not found", sns, sn))
		} else if !sv.Ready {
			crit(fmt.Sprintf("Klio server %s/%s is not ready (%d/%d pods)", sns, sn, sv.PodsReady, sv.Pods))
		}
		k.serverNS = sns
	}

	// --- schedule ---
	if len(schedules) == 0 {
		warn("No ScheduledBackup uses the klio plugin")
	} else {
		sort.Slice(schedules, func(i, j int) bool { return schedules[i].GetName() < schedules[j].GetName() })
		sb := schedules[0]
		k.ScheduleName = sb.GetName()
		k.Schedule, _, _ = unstructured.NestedString(sb.Object, "spec", "schedule")
		k.Suspended, _, _ = unstructured.NestedBool(sb.Object, "spec", "suspend")
		last := parseTime(nested(sb.Object, "status", "lastScheduleTime"))
		next := parseTime(nested(sb.Object, "status", "nextScheduleTime"))
		k.LastSchedule, k.NextSchedule = last, next
		if last != nil && next != nil && next.After(*last) {
			k.IntervalSecs = int64(next.Sub(*last).Seconds())
		}
		if k.Suspended {
			warn("Scheduled backups are suspended")
		}
	}
	every := defaultEvery
	if k.IntervalSecs > 0 {
		every = time.Duration(k.IntervalSecs) * time.Second
	}

	// --- backups ---
	bs := make([]Backup, 0, len(backups))
	for _, b := range backups {
		bs = append(bs, toBackup(b))
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].Created.After(bs[j].Created) }) // newest first
	k.Total = len(bs)
	for i := range bs {
		b := &bs[i]
		switch b.Phase {
		case "completed":
			k.Completed++
			if k.LastSuccess == nil {
				k.LastSuccess = b
			}
		case "failed":
			k.Failed++
			if k.LastFailure == nil {
				k.LastFailure = b
			}
		default:
			k.Running++
		}
	}
	// last finished backup (ignore ones still running)
	for i := range bs {
		if bs[i].Phase == "completed" || bs[i].Phase == "failed" {
			k.LastBackup = &bs[i]
			break
		}
	}
	for i := range bs {
		if bs[i].Phase == "failed" {
			k.ConsecutiveFailures++
		} else if bs[i].Phase == "completed" {
			break
		}
	}
	for i := 0; i < len(bs) && i < recentBackups; i++ {
		k.Recent = append(k.Recent, bs[i])
	}
	n := len(bs)
	if n > historyLen {
		n = historyLen
	}
	for i := n - 1; i >= 0; i-- {
		k.History = append(k.History, bs[i].Phase)
	}

	switch {
	case k.Total == 0:
		warn("No klio backups have been taken yet")
	case k.LastSuccess == nil:
		crit("No successful backup exists")
	default:
		age := now.Sub(k.LastSuccess.reference())
		if age > 2*every+10*time.Minute {
			crit(fmt.Sprintf("Last successful backup is %s old (expected every %s)", round(age), round(every)))
		}
	}
	if k.ConsecutiveFailures >= 2 {
		crit(fmt.Sprintf("Last %d backups failed", k.ConsecutiveFailures))
	} else if k.ConsecutiveFailures == 1 {
		warn("Most recent backup failed: " + shortErr(k.LastFailure.Error))
	}
	return k
}

func (b Backup) reference() time.Time {
	if b.StoppedAt != nil {
		return *b.StoppedAt
	}
	return b.Created
}

func toBackup(b unstructured.Unstructured) Backup {
	st := func(f ...string) string {
		s, _, _ := unstructured.NestedString(b.Object, append([]string{"status"}, f...)...)
		return s
	}
	out := Backup{
		Name:     b.GetName(),
		Created:  b.GetCreationTimestamp().Time.UTC(),
		Phase:    st("phase"),
		BackupID: st("backupId"),
		Pod:      st("instanceID", "podName"),
		BeginWal: st("beginWal"),
		EndWal:   st("endWal"),
		Error:    st("error"),
		Tier1:    st("pluginMetadata", "klio.io/tier1"),
	}
	if out.Phase == "" {
		out.Phase = "pending"
	}
	if v, ok, _ := unstructured.NestedBool(b.Object, "status", "online"); ok {
		out.Online = &v
	}
	out.StartedAt = parseTime(st("startedAt"))
	out.StoppedAt = parseTime(st("stoppedAt"))
	if out.StartedAt != nil && out.StoppedAt != nil {
		out.DurationMs = out.StoppedAt.Sub(*out.StartedAt).Milliseconds()
	}
	return out
}

func buildServer(s unstructured.Unstructured, pods []unstructured.Unstructured) ServerStatus {
	ss := ServerStatus{Namespace: s.GetNamespace(), Name: s.GetName(), Clusters: []string{}}
	ss.Image, _, _ = unstructured.NestedString(s.Object, "spec", "image")
	ss.Mode, _, _ = unstructured.NestedString(s.Object, "spec", "mode")
	ss.Storage, _, _ = unstructured.NestedString(s.Object, "spec", "storage", "pvcTemplate", "resources", "requests", "storage")
	bucket, _, _ := unstructured.NestedString(s.Object, "spec", "tier2", "s3", "bucketName")
	prefix, _, _ := unstructured.NestedString(s.Object, "spec", "tier2", "s3", "prefix")
	if bucket != "" {
		ss.Tier2 = "s3://" + bucket
		if prefix != "" {
			ss.Tier2 += "/" + prefix
		}
	}
	for _, p := range pods {
		if p.GetNamespace() != ss.Namespace || p.GetLabels()["klio.cnpg.io/klio-server"] != ss.Name {
			continue
		}
		ss.Pods++
		if podReady(p) {
			ss.PodsReady++
		}
	}
	ss.Ready = ss.Pods > 0 && ss.PodsReady == ss.Pods
	return ss
}

func podReady(p unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}

// parseAddress turns "name.namespace[.svc...]" or "name" into (name, namespace).
func parseAddress(addr, defaultNS string) (string, string) {
	addr = strings.TrimSpace(addr)
	if i := strings.Index(addr, ":"); i >= 0 {
		addr = addr[:i]
	}
	parts := strings.Split(addr, ".")
	switch {
	case addr == "":
		return "", defaultNS
	case len(parts) == 1:
		return parts[0], defaultNS
	default:
		return parts[0], parts[1]
	}
}

func serverNS(k *Klio, fallback string) string {
	if k.serverNS != "" {
		return k.serverNS
	}
	return fallback
}

func nested(obj map[string]any, f ...string) string {
	s, _, _ := unstructured.NestedString(obj, f...)
	return s
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

func round(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

func shortErr(s string) string {
	s = strings.TrimPrefix(s, "rpc error: code = Unknown desc = ")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	if s == "" {
		return "unknown error"
	}
	return s
}
