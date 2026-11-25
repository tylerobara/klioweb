package restore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// psql runs a query as the postgres OS user (peer auth over the local socket).
func (m *Manager) psql(ctx context.Context, run *Run, pod, db, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := m.exec.Run(ctx, run.Namespace, pod, pgContainer,
		[]string{"psql", "-U", "postgres", "-d", db, "-X", "-A", "-t", "-F", "|", "-c", sql})
	return strings.TrimSpace(out), err
}

// runChecks verifies the restored instance accepts connections, has been
// promoted, and holds data. Every database is inspected.
func (m *Manager) runChecks(ctx context.Context, run *Run, pod string) {
	add := func(name string, ok bool, detail string) {
		run.Checks = append(run.Checks, Check{Name: name, OK: ok, Detail: detail})
	}
	if _, err := m.psql(ctx, run, pod, "postgres", "select 1"); err != nil {
		add("accepts connections", false, err.Error())
		return
	}
	add("accepts connections", true, "")

	rec, err := m.psql(ctx, run, pod, "postgres", "select pg_is_in_recovery()")
	switch {
	case err != nil:
		add("promoted out of recovery", false, err.Error())
	case rec != "f":
		add("promoted out of recovery", false, "still in recovery ("+rec+")")
	default:
		add("promoted out of recovery", true, "")
	}

	dbs, err := m.psql(ctx, run, pod, "postgres",
		"select datname from pg_database where datallowconn and not datistemplate and datname <> 'postgres' order by 1")
	if err != nil {
		add("lists databases", false, err.Error())
		return
	}
	names := strings.Fields(dbs)
	if len(names) == 0 {
		add("has an application database", false, "no databases other than 'postgres'")
		return
	}
	for _, db := range names {
		out, err := m.psql(ctx, run, pod, db, `select
  (select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
    where c.relkind in ('r','p') and n.nspname not in ('pg_catalog','information_schema','pg_toast')),
  pg_size_pretty(pg_database_size(current_database()))`)
		name := "database " + db
		if err != nil {
			add(name, false, err.Error())
			continue
		}
		parts := strings.SplitN(out, "|", 2)
		tables, _ := strconv.Atoi(parts[0])
		detail := fmt.Sprintf("%d tables", tables)
		if len(parts) == 2 {
			detail += ", " + parts[1]
		}
		// An empty database is reported but isn't failed: it may genuinely be empty.
		if tables == 0 {
			detail += " (no user tables)"
		}
		add(name, true, detail)
	}
}

// failedRecoveryPods counts failed pods of the restore cluster's recovery job.
func (m *Manager) failedRecoveryPods(ctx context.Context, run *Run) int {
	pods, err := m.kube.CoreV1().Pods(run.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + run.TestCluster})
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodFailed {
			n++
		}
	}
	return n
}

// diagnostics gathers recent events and the tail of a failed pod's log.
func (m *Manager) diagnostics(ctx context.Context, run *Run) []string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	var out []string
	if evs, err := m.kube.CoreV1().Events(run.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		var lines []string
		for _, e := range evs.Items {
			if strings.HasPrefix(e.InvolvedObject.Name, run.TestCluster) {
				lines = append(lines, fmt.Sprintf("%s %s: %s", e.Type, e.Reason, e.Message))
			}
		}
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		out = append(out, lines...)
	}
	pods, err := m.kube.CoreV1().Pods(run.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + run.TestCluster})
	if err == nil {
		tail := int64(25)
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodFailed {
				continue
			}
			b, err := m.kube.CoreV1().Pods(run.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
			if err == nil {
				out = append(out, "log "+p.Name+":")
				out = append(out, strings.Split(strings.TrimSpace(string(b)), "\n")...)
				break
			}
		}
	}
	return out
}
