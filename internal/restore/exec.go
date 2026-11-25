package restore

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Exec runs a command in a container and returns its stdout.
type Exec interface {
	Run(ctx context.Context, namespace, pod, container string, cmd []string) (string, error)
}

type spdyExec struct {
	cfg *rest.Config
	cs  kubernetes.Interface
}

func NewExec(cfg *rest.Config, cs kubernetes.Interface) Exec { return &spdyExec{cfg: cfg, cs: cs} }

func (e *spdyExec) Run(ctx context.Context, ns, pod, container string, cmd []string) (string, error) {
	req := e.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true},
			scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(e.cfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &errb}); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}
