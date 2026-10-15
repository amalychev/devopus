package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Probe returns a check that the proxy serves host on the host's loopback with a
// certificate chaining to roots. It retries until ctx ends: Envoy applies new
// configuration within moments, but not instantly.
func Probe(roots *x509.CertPool) func(ctx context.Context, host string, ports Ports) error {
	return func(ctx context.Context, host string, ports Ports) error {
		addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(ports.HTTPS))
		c := probeClient(roots, host, addr, 3*time.Second)
		var last error
		for {
			last = probeOnce(ctx, c, host)
			if last == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("https://%s is not served on %s: %w", host, addr, last)
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

func probeOnce(ctx context.Context, c *http.Client, host string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+HealthPath, nil)
	if err != nil {
		return err
	}
	res, err := c.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) {
			return uerr.Unwrap()
		}
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.Header.Get("server") != "devopus" {
		return fmt.Errorf("answered by %q, not the Devopus proxy", res.Header.Get("server"))
	}
	return nil
}

func probeClient(roots *x509.CertPool, host, addr string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		// A redirect is an answer of the module.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ErrNoAnswer: the proxy serves the path, but nothing behind it answers yet.
var ErrNoAnswer = errors.New("no answer")

// Answers returns a single check that the upstream behind host and path answered
// through the proxy, whatever the status: Envoy sets x-envoy-upstream-service-time
// only on responses that came from upstream, not on its own 503.
func Answers(roots *x509.CertPool) func(ctx context.Context, host, path string, ports Ports) error {
	return func(ctx context.Context, host, path string, ports Ports) error {
		addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(ports.HTTPS))
		// Long enough for a dev server that compiles on the first request.
		c := probeClient(roots, host, addr, 60*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
		if err != nil {
			return err
		}
		res, err := c.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		if res.Header.Get("x-envoy-upstream-service-time") == "" {
			return fmt.Errorf("%w (%s)", ErrNoAnswer, res.Status)
		}
		return nil
	}
}
