package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tylerobara/klioweb/internal/collector"
)

type fakeSrc struct{ snap *collector.Snapshot }

func (f *fakeSrc) Snapshot(context.Context) (*collector.Snapshot, error) { return f.snap, nil }

func snapWith(phases ...string) *collector.Snapshot {
	k := &collector.Klio{Health: collector.HealthOK}
	for i := len(phases) - 1; i >= 0; i-- { // newest first
		k.Recent = append(k.Recent, collector.Backup{Name: "b" + string(rune('0'+i)), Phase: phases[i]})
	}
	return &collector.Snapshot{Clusters: []collector.ClusterStatus{{Namespace: "ns", Name: "db", Klio: k}}}
}

func TestModes(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		want int
	}{{OnFailure, 1}, {OnSuccess, 1}, {OnBoth, 2}} {
		var got int
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { got++ }))
		src := &fakeSrc{snap: snapWith("completed")} // pre-existing history: must not notify
		n := New(tc.mode, src, NewWebhook(srv.URL))
		n.tick(context.Background())
		if got != 0 {
			t.Fatalf("%s: history was notified", tc.mode)
		}
		src.snap = snapWith("completed", "completed", "failed") // one new success, one new failure
		n.tick(context.Background())
		n.tick(context.Background()) // no duplicates
		if got != tc.want {
			t.Fatalf("%s: got %d notifications, want %d", tc.mode, got, tc.want)
		}
		srv.Close()
	}
}

func TestPushoverRequest(t *testing.T) {
	var form url.Values
	var path, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		path, ctype = r.URL.Path, r.Header.Get("Content-Type")
		w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()
	p := NewPushover("tok", "usr")
	p.Endpoint = srv.URL + "/1/messages.json"
	p.Sound, p.Device, p.LinkURL = "siren", "phone", "https://klio.example"
	p.FailurePriority, p.Retry, p.Expire = 2, 120, 600

	src := &fakeSrc{snap: snapWith("completed")}
	n := New(OnBoth, src, p)
	n.tick(context.Background())
	snap := snapWith("completed", "failed")
	snap.Clusters[0].Klio.Recent[0].Error = "boom\nstack"
	src.snap = snap
	n.tick(context.Background())

	if path != "/1/messages.json" || ctype != "application/x-www-form-urlencoded" {
		t.Fatalf("bad request: %s %s", path, ctype)
	}
	want := map[string]string{"token": "tok", "user": "usr", "priority": "2", "retry": "120", "expire": "600",
		"sound": "siren", "device": "phone", "url": "https://klio.example",
		"title": "Klio backup failed: ns/db", "message": "b1\nboom\nstack"}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
}

func TestPushoverNoRetryOnBadToken(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(400)
		w.Write([]byte(`{"token":"invalid","status":0}`))
	}))
	defer srv.Close()
	p := NewPushover("bad", "usr")
	p.Endpoint = srv.URL
	src := &fakeSrc{snap: snapWith("completed")}
	n := New(OnFailure, src, p)
	n.tick(context.Background())
	src.snap = snapWith("completed", "failed")
	n.tick(context.Background())
	n.tick(context.Background())
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (4xx is permanent)", calls)
	}
}

func TestRetryOnlyFailedSender(t *testing.T) {
	var okCalls, flakyCalls int
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { okCalls++ }))
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flakyCalls++
		if flakyCalls == 1 {
			w.WriteHeader(503)
		}
	}))
	defer ok.Close()
	defer flaky.Close()
	src := &fakeSrc{snap: snapWith("completed")}
	n := New(OnFailure, src, NewWebhook(ok.URL), &Pushover{Endpoint: flaky.URL, Client: http.DefaultClient})
	n.tick(context.Background())
	src.snap = snapWith("completed", "failed")
	n.tick(context.Background())
	n.tick(context.Background())
	if okCalls != 1 || flakyCalls != 2 {
		t.Fatalf("ok=%d flaky=%d, want 1 and 2", okCalls, flakyCalls)
	}
}

func TestValidateAndMode(t *testing.T) {
	p := NewPushover("a", "b")
	p.FailurePriority = 3
	if p.Validate() == nil {
		t.Fatal("priority 3 should be invalid")
	}
	p.FailurePriority, p.Retry = 2, 10
	if p.Validate() == nil {
		t.Fatal("priority 2 with retry 10 should be invalid")
	}
	if _, err := ParseMode("nope"); err == nil {
		t.Fatal("expected error")
	}
	if m, _ := ParseMode(""); m != OnFailure {
		t.Fatal("default should be failure")
	}
}
