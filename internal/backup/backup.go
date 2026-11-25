// Package backup lets a logged-in user trigger an on-demand Klio backup by
// creating a CloudNativePG Backup object for a cluster.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	gvrClusters = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	gvrBackups  = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
)

const (
	klioPlugin  = "klio.cnpg.io"
	LabelManual = "klioweb.obara.dev/manual-backup"
	AnnoUser    = "klioweb.obara.dev/triggered-by"
)

var (
	ErrInvalid   = errors.New("invalid request")
	ErrForbidden = errors.New("namespace not enabled for manual backups")
	ErrBusy      = errors.New("a backup is already running for this cluster")
)

// Guard is the subset of the auth package the handlers need.
type Guard interface {
	RequireLogin(http.Handler) http.Handler
	UserName(*http.Request) (string, bool)
}

type Manager struct {
	namespaces []string
	dyn        dynamic.Interface
	now        func() time.Time
	mu         sync.Mutex // serialises the busy-check and create
}

func New(namespaces []string, dyn dynamic.Interface) *Manager {
	return &Manager{namespaces: namespaces, dyn: dyn, now: time.Now}
}

func (m *Manager) Namespaces() []string { return m.namespaces }

// Trigger creates a Backup for the cluster and returns its name.
func (m *Manager) Trigger(ctx context.Context, user, ns, cluster string) (string, error) {
	if ns == "" || cluster == "" {
		return "", fmt.Errorf("%w: namespace and cluster are required", ErrInvalid)
	}
	if !slices.Contains(m.namespaces, ns) {
		return "", ErrForbidden
	}
	cl, err := m.dyn.Resource(gvrClusters).Namespace(ns).Get(ctx, cluster, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: cluster %s/%s: %v", ErrInvalid, ns, cluster, err)
	}
	if !usesKlio(cl) {
		return "", fmt.Errorf("%w: cluster %s/%s does not use the klio plugin", ErrInvalid, ns, cluster)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	list, err := m.dyn.Resource(gvrBackups).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, b := range list.Items {
		if c, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name"); c != cluster {
			continue
		}
		switch p, _, _ := unstructured.NestedString(b.Object, "status", "phase"); p {
		case "completed", "failed":
		default:
			return "", fmt.Errorf("%w (%s)", ErrBusy, b.GetName())
		}
	}

	name := fmt.Sprintf("%s-klio-ui-%s", cluster, m.now().UTC().Format("20060102150405"))
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"labels":      map[string]any{LabelManual: "true"},
			"annotations": map[string]any{AnnoUser: user},
		},
		"spec": map[string]any{
			"cluster":             map[string]any{"name": cluster},
			"method":              "plugin",
			"pluginConfiguration": map[string]any{"name": klioPlugin},
			"target":              "primary",
		},
	}}
	if _, err := m.dyn.Resource(gvrBackups).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	log.Printf("backup: %s requested manual backup %s/%s", user, ns, name)
	return name, nil
}

func usesKlio(cl *unstructured.Unstructured) bool {
	plugins, _, _ := unstructured.NestedSlice(cl.Object, "spec", "plugins")
	for _, p := range plugins {
		pm, _ := p.(map[string]any)
		if pm["name"] != klioPlugin {
			continue
		}
		if en, ok := pm["enabled"].(bool); ok && !en {
			return false
		}
		return true
	}
	return false
}

type request struct {
	Namespace string `json:"namespace"`
	Cluster   string `json:"cluster"`
}

// Register mounts the API. GET is public; POST requires login.
func (m *Manager) Register(mux *http.ServeMux, g Guard) {
	mux.HandleFunc("GET /api/backups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "namespaces": m.namespaces})
	})
	mux.Handle("POST /api/backups", g.RequireLogin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		user, _ := g.UserName(r)
		name, err := m.Trigger(r.Context(), user, strings.TrimSpace(req.Namespace), strings.TrimSpace(req.Cluster))
		switch {
		case errors.Is(err, ErrInvalid):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrForbidden):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrBusy):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case err != nil:
			log.Printf("backup: create: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create backup"})
		default:
			writeJSON(w, http.StatusAccepted, map[string]string{"name": name})
		}
	})))
}

func RegisterDisabled(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/backups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "namespaces": []string{}})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
