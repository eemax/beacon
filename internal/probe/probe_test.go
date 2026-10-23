package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eemax/beacon/internal/spec"
)

func find(cs []spec.Check, name string) spec.Check {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return spec.Check{}
}

func TestHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	// httptest's certificate isn't trusted: the probe must still report its dates.
	g, c := Run(context.Background(), Probe{URL: srv.URL, Expect: []int{200, 401}}, time.Now())
	if find(c, "certificate").Status != spec.Fail || !strings.Contains(find(c, "certificate").Detail, "certificate") {
		t.Fatalf("untrusted certificate: %+v", c)
	}
	var expires bool
	for _, m := range g {
		expires = expires || (m.Key == "cert.expires" && m.Value > 0 && m.Unit == "s")
	}
	if !expires || find(c, "reachable").Status != spec.Fail {
		t.Fatalf("gauges %+v checks %+v", g, c)
	}
}

func TestHTTPExpect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", 302) }))
	defer srv.Close()
	_, c := Run(context.Background(), Probe{URL: srv.URL}, time.Now())
	if r := find(c, "reachable"); r.Status != spec.OK || r.Detail != "HTTP 302" {
		t.Fatalf("redirect not followed and counted as up: %+v", r)
	}
	_, c = Run(context.Background(), Probe{URL: srv.URL, Expect: []int{200}}, time.Now())
	if r := find(c, "reachable"); r.Status != spec.Fail || r.Detail != "HTTP 302" {
		t.Fatalf("unexpected status: %+v", r)
	}
	if len(c) != 1 {
		t.Fatal("http probe got a certificate check")
	}
}

func TestTCP(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	g, c := Run(context.Background(), Probe{TCP: addr}, time.Now())
	if c[0].Status != spec.OK || len(g) != 1 || g[0].Labels["probe"] != addr {
		t.Fatalf("open port: %+v %+v", g, c)
	}
	l.Close()
	_, c = Run(context.Background(), Probe{TCP: addr}, time.Now())
	if c[0].Status != spec.Fail || c[0].Detail == "" {
		t.Fatalf("closed port: %+v", c)
	}
}

func TestValidate(t *testing.T) {
	for _, p := range []Probe{{}, {URL: "ftp://x"}, {URL: "https://u:p@x/"}, {TCP: "nohost"}, {TCP: "x:22", Expect: []int{200}}, {URL: "https://x/", Expect: []int{42}}, {URL: "https://x/", Name: "a,b"}} {
		if p.Validate() == nil {
			t.Errorf("%+v passed", p)
		}
	}
	if err := (Probe{URL: "https://vps.example.com/", Expect: []int{401}}).Validate(); err != nil {
		t.Error(err)
	}
}
