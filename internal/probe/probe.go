// Package probe checks a service the way a client would: an HTTP(S) request (status code,
// latency, and for HTTPS the certificate it was served) or a TCP connect. A definition's
// probes become gauges and checks labelled probe=<name>, so the hub shows them as a table.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/eemax/beacon/internal/spec"
)

type Probe struct {
	Name   string `json:"name,omitempty"`   // the table row (default: the host, or host:port)
	URL    string `json:"url,omitempty"`    // http:// or https://
	TCP    string `json:"tcp,omitempty"`    // host:port, connect only
	Expect []int  `json:"expect,omitempty"` // HTTP status codes that count as up (default 200-399)
}

const (
	Timeout  = 10 * time.Second
	CertWarn = 14 * 24 * time.Hour
	CertFail = 7 * 24 * time.Hour
	MaxCount = 10
)

// Label is the probe's row name.
func (p Probe) Label() string {
	if p.Name != "" {
		return p.Name
	}
	if p.TCP != "" {
		return p.TCP
	}
	if u, err := url.Parse(p.URL); err == nil {
		return u.Host
	}
	return p.URL
}

func (p Probe) Validate() error {
	switch {
	case (p.URL == "") == (p.TCP == ""):
		return errors.New("a probe has either url or tcp")
	case p.URL != "":
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(p.URL) > 500 {
			return fmt.Errorf("url %q: want an http(s) URL", p.URL)
		}
		if u.User != nil {
			return errors.New("url: no credentials in probe URLs")
		}
	default:
		if _, port, err := net.SplitHostPort(p.TCP); err != nil || port == "" {
			return fmt.Errorf("tcp %q: want host:port", p.TCP)
		}
		if len(p.Expect) > 0 {
			return errors.New("expect: only for url probes")
		}
	}
	for _, c := range p.Expect {
		if c < 100 || c > 599 {
			return fmt.Errorf("expect: %d is not an HTTP status", c)
		}
	}
	l := p.Label()
	if l == "" || len([]rune(l)) > 64 || strings.ContainsAny(l, ",={}") {
		return fmt.Errorf("name %q: 1-64 characters without , = { }", l)
	}
	return nil
}

// Run probes once. The gauges are latency (ms) and, for HTTPS, the time left on the
// certificate (s); the checks are "reachable" and "certificate".
func Run(ctx context.Context, p Probe, now time.Time) ([]spec.Metric, []spec.Check) {
	labels := spec.Labels{"probe": p.Label()}
	reachable := spec.Check{Name: "reachable", Status: spec.OK, Labels: labels}
	var gauges []spec.Metric
	var checks []spec.Check
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	start := time.Now()

	if p.TCP != "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.TCP)
		if err != nil {
			reachable.Status, reachable.Detail = spec.Fail, why(err)
		} else {
			conn.Close()
			gauges = append(gauges, latency(start, labels))
		}
		return gauges, append(checks, reachable)
	}

	var served *x509.Certificate
	client := &http.Client{
		Timeout:       Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: Timeout, ResponseHeaderTimeout: Timeout,
		},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	req.Header.Set("User-Agent", "beacon-probe")
	res, err := client.Do(req)
	if err != nil {
		reachable.Status, reachable.Detail = spec.Fail, why(err)
	} else {
		res.Body.Close()
		gauges = append(gauges, latency(start, labels))
		if res.TLS != nil && len(res.TLS.PeerCertificates) > 0 {
			served = res.TLS.PeerCertificates[0]
		}
		if !expected(p.Expect, res.StatusCode) {
			reachable.Status, reachable.Detail = spec.Fail, fmt.Sprintf("HTTP %d", res.StatusCode)
		} else {
			reachable.Detail = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
	}
	checks = append(checks, reachable)

	if strings.HasPrefix(p.URL, "https://") {
		cert := spec.Check{Name: "certificate", Status: spec.OK, Labels: labels}
		var invalid error
		if served == nil {
			// The request failed: was it the certificate? Look at what's served without
			// verifying, so an expired or wrong certificate still shows its dates.
			var verr *tls.CertificateVerificationError
			if errors.As(err, &verr) {
				invalid = verr.Err
				if len(verr.UnverifiedCertificates) > 0 {
					served = verr.UnverifiedCertificates[0]
				}
			}
		}
		switch {
		case served == nil:
			cert.Status, cert.Detail = spec.Unknown, "no certificate seen"
		default:
			left := served.NotAfter.Sub(now)
			gauges = append(gauges, spec.Metric{Key: "cert.expires", Label: "Certificate expires in", Value: max(0, left.Truncate(time.Second).Seconds()), Unit: "s", Labels: labels, Primary: true})
			cert.Detail = "valid until " + served.NotAfter.UTC().Format("2006-01-02")
			switch {
			case invalid != nil:
				cert.Status, cert.Detail = spec.Fail, why(invalid)
			case left <= CertFail:
				cert.Status = spec.Fail
			case left <= CertWarn:
				cert.Status = spec.Warn
			}
		}
		checks = append(checks, cert)
	}
	return gauges, checks
}

func latency(start time.Time, labels spec.Labels) spec.Metric {
	ms := float64(time.Since(start).Microseconds()) / 1000
	return spec.Metric{Key: "latency", Label: "Response time", Value: float64(int64(ms*10)) / 10, Unit: "ms", Labels: labels}
}

func expected(codes []int, got int) bool {
	if len(codes) == 0 {
		return got >= 200 && got < 400
	}
	return slices.Contains(codes, got)
}

// why turns a dial/HTTP/TLS error into a short reason.
func why(err error) string {
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return fmt.Sprintf("no answer within %s", Timeout)
	case errors.As(err, &dns):
		return "DNS: " + dns.Err
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && !strings.Contains(msg, "certificate") {
		msg = msg[i+2:]
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
