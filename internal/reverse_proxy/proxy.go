package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Shbhom/llm-proxy/internal/pool"
	"github.com/Shbhom/llm-proxy/internal/types"
)

type ReverseProxy struct {
	upstream *url.URL
	client   *http.Transport
	pl       *pool.Pool
	st       types.Store
	cfg      *types.Config
	rp       *httputil.ReverseProxy
}

func NewReverseProxy(cfg *types.Config, pl *pool.Pool, st types.Store) (http.Handler, error) {
	u, err := url.Parse(cfg.Upstream.BaseURL)
	if err != nil {
		return nil, err
	}

	// Diagnostic-safe transport: no env proxy, H1 only, strict timeouts.
	tr := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DialContext:         (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 8 * time.Second,

		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
	}

	h := &ReverseProxy{
		upstream: u,
		client:   tr,
		pl:       pl,
		st:       st,
		cfg:      cfg,
	}

	rp := &httputil.ReverseProxy{
		Director:      h.director, // no-op
		Transport:     tr,
		FlushInterval: 150 * time.Millisecond,
		ModifyResponse: func(resp *http.Response) error {
			keyID, _ := resp.Request.Context().Value(ctxKeyID{}).(string)
			hdr := lowerCaseHeaders(resp.Header)
			h.pl.ReconcileFromHeaders(keyID, hdr, resp.StatusCode)
			log.Printf("[resp] %s %s status=%d", resp.Request.Method, resp.Request.URL.String(), resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[err ] %s %s err=%v", r.Method, r.URL.Path, err)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
	h.rp = rp
	return h, nil
}

// no-op Director; we rewrite in ServeHTTP
func (h *ReverseProxy) director(req *http.Request) {}

type ctxKeyID struct{}

func (h *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	key, readyAt := h.pl.Select(0)

	// Optional queue wait
	if h.cfg.RateLimits.Queue.Enabled && readyAt.After(time.Now()) {
		wait := time.Until(readyAt)
		maxWait := time.Duration(h.cfg.RateLimits.Queue.MaxWaitMs) * time.Millisecond
		if wait > maxWait {
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()))
			http.Error(w, "rate-limited; try later", http.StatusTooManyRequests)
			return
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			w.WriteHeader(499)
			_, _ = w.Write([]byte("client cancelled"))
			return
		}
	}

	log.Printf("[req] %s %s key=%s ready_in=%v", r.Method, r.URL.Path, key.Name, time.Until(readyAt).Truncate(time.Millisecond))
	h.pl.ProvisionalConsume(key.ID, 0)

	// Build upstream URL
	u := *h.upstream
	u.Path = singleJoin(h.upstream.Path, r.URL.Path)
	u.RawQuery = r.URL.RawQuery

	// --- Buffer the body to avoid streaming deadlocks on POST/PUT/PATCH ---
	var bodyCopy []byte
	if r.Body != nil {
		var err error
		bodyCopy, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body error", http.StatusBadRequest)
			return
		}
		r.Body.Close()
	}

	// Clone request we pass to ReverseProxy
	out := r.Clone(context.WithValue(r.Context(), ctxKeyID{}, key.ID))
	out.URL = &u
	out.Host = u.Host

	filterHopByHop(out.Header)
	out.Header.Del("Authorization")
	out.Header.Set("Authorization", "Bearer "+key.Value)
	if out.Header.Get("X-Request-Id") == "" {
		out.Header.Set("X-Request-Id", randomID())
	}

	// Replace body with buffered copy and set explicit Content-Length
	out.Body = io.NopCloser(bytes.NewReader(bodyCopy))
	out.ContentLength = int64(len(bodyCopy))
	if out.ContentLength >= 0 {
		out.Header.Set("Content-Length", strconv.FormatInt(out.ContentLength, 10))
	} else {
		out.Header.Del("Content-Length")
	}

	// Hard per-request timeout
	ctx, cancel := context.WithTimeout(out.Context(), 35*time.Second)
	defer cancel()
	out = out.WithContext(ctx)

	h.rp.ServeHTTP(w, out)

	_ = start // hook for future latency metrics
}

// helpers

func singleJoin(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func filterHopByHop(h http.Header) {
	h.Del("Proxy-Connection")
	h.Del("Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Authenticate")
	h.Del("Proxy-Authorization")
	h.Del("Te")
	h.Del("Trailer")
	h.Del("Transfer-Encoding")
	h.Del("Upgrade")
}

func lowerCaseHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

func randomID() string {
	// cheap unique id; fine for logs
	var b [12]byte
	if _, err := io.ReadFull(randReader{}, b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}

// minimal rand reader using time jitter
type randReader struct{}

func (randReader) Read(p []byte) (int, error) {
	br := bufio.NewReader(strings.NewReader(time.Now().Format(time.RFC3339Nano)))
	return br.Read(p)
}
