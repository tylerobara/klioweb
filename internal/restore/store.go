package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// store keeps one ConfigMap per run in klioweb's own namespace.
type store struct {
	kube kubernetes.Interface
	ns   string
}

func cmName(id string) string { return "klioweb-restore-" + id }

func (s *store) save(ctx context.Context, r Run) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: cmName(r.ID), Namespace: s.ns,
			Labels: map[string]string{
				"app.kubernetes.io/name": "klioweb", LabelRuns: "true",
			},
		},
		Data: map[string]string{"run.json": string(b)},
	}
	api := s.kube.CoreV1().ConfigMaps(s.ns)
	if _, err := api.Update(ctx, cm, metav1.UpdateOptions{}); apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, cm, metav1.CreateOptions{})
		return err
	} else {
		return err
	}
}

// list returns all runs, newest first.
func (s *store) list(ctx context.Context) ([]Run, error) {
	l, err := s.kube.CoreV1().ConfigMaps(s.ns).List(ctx, metav1.ListOptions{LabelSelector: LabelRuns + "=true"})
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(l.Items))
	for _, cm := range l.Items {
		var r Run
		if json.Unmarshal([]byte(cm.Data["run.json"]), &r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *store) get(ctx context.Context, id string) (*Run, error) {
	cm, err := s.kube.CoreV1().ConfigMaps(s.ns).Get(ctx, cmName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal([]byte(cm.Data["run.json"]), &r); err != nil {
		return nil, fmt.Errorf("corrupt run %s: %w", id, err)
	}
	return &r, nil
}

// prune keeps the newest `keep` finished runs per source cluster.
func (s *store) prune(ctx context.Context, keep int) {
	runs, err := s.list(ctx)
	if err != nil || keep <= 0 {
		return
	}
	seen := map[string]int{}
	for _, r := range runs { // newest first
		if !r.Terminal() {
			continue
		}
		k := r.Namespace + "/" + r.Cluster
		seen[k]++
		if seen[k] > keep {
			s.kube.CoreV1().ConfigMaps(s.ns).Delete(ctx, cmName(r.ID), metav1.DeleteOptions{})
		}
	}
}
