package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tylerobara/klioweb/internal/auth"
	"github.com/tylerobara/klioweb/internal/collector"
	"github.com/tylerobara/klioweb/internal/notify"
	"github.com/tylerobara/klioweb/internal/backup"
	"github.com/tylerobara/klioweb/internal/restore"
)

//go:embed web
var webFS embed.FS

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg, err := restConfig()
	if err != nil {
		log.Fatalf("kubernetes config: %v", err)
	}
	cfg.QPS, cfg.Burst = 20, 40
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}

	ttl := 10 * time.Second
	if v := os.Getenv("CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			ttl = d
		}
	}
	col := collector.New(client, ttl)

	ctx, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	if senders := notifiers(); len(senders) > 0 {
		mode, err := notify.ParseMode(os.Getenv("NOTIFY_ON"))
		if err != nil {
			log.Fatal(err)
		}
		go notify.New(mode, col, senders...).Run(ctx, 30*time.Second)
	}

	mux := http.NewServeMux()
	var authn *auth.Auth
	if cfg := oidcConfig(); cfg != nil {
		a, err := auth.New(ctx, *cfg)
		if err != nil {
			log.Fatal(err)
		}
		a.Register(mux)
		a.Version = version
		authn = a
		log.Printf("OIDC login enabled (issuer %s)", cfg.IssuerURL)
	} else {
		auth.RegisterDisabled(mux)
	}

	if os.Getenv("RESTORE_ENABLED") == "true" {
		if authn == nil {
			log.Fatal("RESTORE_ENABLED requires OIDC login to be configured")
		}
		kube, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			log.Fatalf("kubernetes client: %v", err)
		}
		ns := os.Getenv("POD_NAMESPACE")
		if ns == "" {
			log.Fatal("POD_NAMESPACE is required when restore tests are enabled")
		}
		timeout := 30 * time.Minute
		if v := os.Getenv("RESTORE_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Fatalf("RESTORE_TIMEOUT: %v", err)
			}
			timeout = d
		}
		mgr := restore.New(ctx, restore.Config{
			Namespaces:   splitList(os.Getenv("RESTORE_NAMESPACES")),
			OwnNamespace: ns,
			Timeout:      timeout,
			Keep:         envInt("RESTORE_KEEP", 20),
		}, client, kube, restore.NewExec(cfg, kube))
		mgr.Recover(ctx)
		mgr.Register(mux, authn)
		log.Printf("restore tests enabled for namespaces %v", mgr.Namespaces())
	} else {
		restore.RegisterDisabled(mux)
	}

	if os.Getenv("BACKUP_ENABLED") == "true" {
		if authn == nil {
			log.Fatal("BACKUP_ENABLED requires OIDC login to be configured")
		}
		bm := backup.New(splitList(os.Getenv("BACKUP_NAMESPACES")), client)
		bm.Register(mux, authn)
		log.Printf("manual backups enabled for namespaces %v", bm.Namespaces())
	} else {
		backup.RegisterDisabled(mux)
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		snap, err := col.Snapshot(r.Context())
		if err != nil {
			log.Printf("collect: %v", err)
			http.Error(w, `{"error":"failed to read cluster state"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(snap)
	})
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServerFS(sub))

	addr := ":8080"
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		addr = v
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("klioweb listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopBG()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}

// restConfig uses in-cluster config, falling back to the local kubeconfig for development.
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// notifiers builds the configured notification senders from the environment.
func notifiers() []notify.Sender {
	var out []notify.Sender
	if u := os.Getenv("NOTIFY_WEBHOOK_URL"); u != "" {
		out = append(out, notify.NewWebhook(u))
	}
	if tok, user := os.Getenv("PUSHOVER_TOKEN"), os.Getenv("PUSHOVER_USER"); tok != "" || user != "" {
		if tok == "" || user == "" {
			log.Fatal("pushover needs both PUSHOVER_TOKEN and PUSHOVER_USER")
		}
		p := notify.NewPushover(tok, user)
		p.Device, p.Sound = os.Getenv("PUSHOVER_DEVICE"), os.Getenv("PUSHOVER_SOUND")
		p.LinkURL = os.Getenv("NOTIFY_LINK_URL")
		p.FailurePriority = envInt("PUSHOVER_PRIORITY_FAILURE", p.FailurePriority)
		p.SuccessPriority = envInt("PUSHOVER_PRIORITY_SUCCESS", p.SuccessPriority)
		p.Retry = envInt("PUSHOVER_RETRY", p.Retry)
		p.Expire = envInt("PUSHOVER_EXPIRE", p.Expire)
		if err := p.Validate(); err != nil {
			log.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: %q is not an integer", name, v)
	}
	return n
}

// oidcConfig returns nil unless OIDC_ISSUER_URL is set.
func oidcConfig() *auth.Config {
	issuer := os.Getenv("OIDC_ISSUER_URL")
	if issuer == "" {
		return nil
	}
	ttl := 12 * time.Hour
	if v := os.Getenv("OIDC_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("OIDC_SESSION_TTL: %v", err)
		}
		ttl = d
	}
	return &auth.Config{
		IssuerURL:     issuer,
		ClientID:      os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret:  os.Getenv("OIDC_CLIENT_SECRET"),
		ExternalURL:   os.Getenv("OIDC_EXTERNAL_URL"),
		Scopes:        splitList(os.Getenv("OIDC_SCOPES")),
		GroupsClaim:   os.Getenv("OIDC_GROUPS_CLAIM"),
		AllowedGroups: splitList(os.Getenv("OIDC_ALLOWED_GROUPS")),
		AllowedEmails: splitList(os.Getenv("OIDC_ALLOWED_EMAILS")),
		SessionTTL:    ttl,
		SessionSecret: []byte(os.Getenv("OIDC_SESSION_SECRET")),
	}
}

// splitList splits on commas and whitespace, dropping empties.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
}
