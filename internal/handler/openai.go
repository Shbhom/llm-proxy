package handler

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Shbhom/llm-proxy/internal/pool"
	"github.com/Shbhom/llm-proxy/internal/types"
)

const statusClientClosed = 499 // nginx-style

type reconEvent struct {
	keyID  string
	hdr    map[string]string
	status int
}

// OpenAIHandler exposes OpenAI-compatible endpoints and calls Groq upstream.
type OpenAIHandler struct {
	upstreamBase *url.URL
	httpClient   *http.Client
	pl           *pool.Pool
	st           types.Store
	cfg          *types.Config

	reconCh chan reconEvent
}

func NewOpenAIHandler(cfg *types.Config, pl *pool.Pool, st types.Store) (http.Handler, error) {
	base := strings.TrimRight(cfg.Upstream.BaseURL, "/") + "/openai/v1"
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:               nil,   // ignore env proxies while debugging
		ForceAttemptHTTP2:   false, // can flip true later
		DialContext:         (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 8 * time.Second,

		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
	}

	h := &OpenAIHandler{
		upstreamBase: u,
		httpClient:   &http.Client{Transport: tr},
		pl:           pl,
		st:           st,
		cfg:          cfg,
		reconCh:      make(chan reconEvent, 1024), // drop instead of blocking if full
	}
	go h.reconWorker()

	return &router{h}, nil
}

// tiny router
type router struct{ *OpenAIHandler }

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/openai/v1/models":
		rt.listModels(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/openai/v1/chat/completions":
		rt.chatCompletions(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ===== GET /openai/v1/models =====

func (h *OpenAIHandler) listModels(w http.ResponseWriter, r *http.Request) {
	reqID := shortID()
	start := time.Now()
	log.Printf("[%s] GET %s: start", reqID, r.URL.Path)

	key, readyAt := h.pl.Select(0)
	log.Printf("[%s] GET %s: selected key=%s readyIn=%v", reqID, r.URL.Path, key.Name, time.Until(readyAt).Truncate(time.Millisecond))
	if !h.waitIfNeeded(reqID, w, r, readyAt) {
		return
	}
	h.pl.ProvisionalConsume(key.ID, 0)

	u := *h.upstreamBase
	u.Path = "/openai/v1/models"
	u.RawQuery = r.URL.RawQuery

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	copyHeadersSet(req.Header, r.Header)
	stripHopByHop(req.Header)
	req.Header.Set("Authorization", "Bearer "+key.Value)
	req.Host = u.Host

	attachTrace(reqID, req)

	log.Printf("[%s] GET %s: sending upstream -> %s", reqID, r.URL.Path, u.String())
	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[%s] GET %s: upstream error: %v", reqID, r.URL.Path, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] GET %s: upstream status=%d", reqID, r.URL.Path, resp.StatusCode)

	// async, non-blocking
	h.reconcileAsync(key.ID, resp.Header, resp.StatusCode)

	copyHeadersSet(w.Header(), resp.Header)
	stripHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	log.Printf("[%s] GET %s: done status=%d bytes=%d dur=%v", reqID, r.URL.Path, resp.StatusCode, n, time.Since(start))
}

// ===== POST /openai/v1/chat/completions =====

func (h *OpenAIHandler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := shortID()
	start := time.Now()
	log.Printf("[%s] POST %s: start", reqID, r.URL.Path)

	// 1) Choose key (and possibly wait) BEFORE reading client body.
	key, readyAt := h.pl.Select(0)
	wait := time.Until(readyAt).Truncate(time.Millisecond)
	log.Printf("[%s] POST %s: selected key=%s readyIn=%v", reqID, r.URL.Path, key.Name, wait)
	if !h.waitIfNeeded(reqID, w, r, readyAt) {
		return
	}
	h.pl.ProvisionalConsume(key.ID, 0)

	// 2) Read & (optionally) tweak request body.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[%s] POST %s: read body error: %v", reqID, r.URL.Path, err)
		http.Error(w, "read body error", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	log.Printf("[%s] POST %s: body bytes=%d", reqID, r.URL.Path, len(raw))

	var meta struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(raw, &meta)
	if strings.HasPrefix(meta.Model, "groq/") {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err == nil {
			m["model"] = strings.TrimPrefix(meta.Model, "groq/")
			raw, _ = json.Marshal(m)
			log.Printf("[%s] POST %s: model rewrite %q -> %q", reqID, r.URL.Path, meta.Model, m["model"])
		}
	}

	// 3) Build & send upstream
	u := *h.upstreamBase
	u.Path = "/openai/v1/chat/completions"
	u.RawQuery = r.URL.RawQuery

	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	copyHeadersSet(req.Header, r.Header)
	stripHopByHop(req.Header)
	req.Header.Set("Authorization", "Bearer "+key.Value)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	req.Host = u.Host

	attachTrace(reqID, req)
	log.Printf("[%s] POST %s: sending upstream -> %s (stream=false)", reqID, r.URL.Path, u.String())
	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[%s] POST %s: upstream error: %v", reqID, r.URL.Path, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] POST %s: upstream status=%d", reqID, r.URL.Path, resp.StatusCode)

	// async, non-blocking
	h.reconcileAsync(key.ID, resp.Header, resp.StatusCode)

	// 4) send headers immediately; then stream body to client
	copyHeadersSet(w.Header(), resp.Header)
	stripHopByHop(w.Header())
	w.Header().Del("Content-Length") // stream chunked
	log.Printf("[%s] POST %s: writing headers to client now", reqID, r.URL.Path)
	w.WriteHeader(resp.StatusCode)

	var total int64
	if fl, ok := w.(http.Flusher); ok {
		br := bufio.NewReader(resp.Body)
		buf := make([]byte, 32*1024)
		last := time.Now()
		for {
			n, er := br.Read(buf)
			if n > 0 {
				wn, ew := w.Write(buf[:n])
				total += int64(wn)
				fl.Flush()
				if ew != nil {
					log.Printf("[%s] POST %s: client write err after %d bytes: %v", reqID, r.URL.Path, total, ew)
					break
				}
				if time.Since(last) > 2*time.Second {
					log.Printf("[%s] POST %s: streaming... bytes=%d", reqID, r.URL.Path, total)
					last = time.Now()
				}
			}
			if er != nil {
				if er != io.EOF {
					log.Printf("[%s] POST %s: upstream read err after %d bytes: %v", reqID, r.URL.Path, total, er)
				}
				break
			}
		}
	} else {
		n, err := io.Copy(w, resp.Body)
		total = n
		if err != nil {
			log.Printf("[%s] POST %s: copy err after %d bytes: %v", reqID, r.URL.Path, total, err)
		}
	}

	log.Printf("[%s] POST %s: done status=%d bytes=%d dur=%v", reqID, r.URL.Path, resp.StatusCode, total, time.Since(start))
}

// ===== async reconcile =====

func (h *OpenAIHandler) reconcileAsync(keyID string, hdr http.Header, status int) {
	ev := reconEvent{keyID: keyID, hdr: lower(hdr), status: status}
	select {
	case h.reconCh <- ev:
	default:
		log.Printf("[recon] queue full; dropping update for key=%s", keyID)
	}
}

func (h *OpenAIHandler) reconWorker() {
	for ev := range h.reconCh {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[recon] panic: %v", r)
				}
			}()
			start := time.Now()
			h.pl.ReconcileFromHeaders(ev.keyID, ev.hdr, ev.status)
			log.Printf("[recon] applied key=%s status=%d in %v", ev.keyID, ev.status, time.Since(start))
		}()
	}
}

// ===== helpers =====

func (h *OpenAIHandler) waitIfNeeded(reqID string, w http.ResponseWriter, r *http.Request, readyAt time.Time) bool {
	if h.cfg.RateLimits.Queue.Enabled && readyAt.After(time.Now()) {
		wait := time.Until(readyAt)
		maxWait := time.Duration(h.cfg.RateLimits.Queue.MaxWaitMs) * time.Millisecond
		log.Printf("[%s] WAIT %s: need to wait %v (max=%v)", reqID, r.URL.Path, wait.Truncate(time.Millisecond), maxWait)
		if wait > maxWait {
			w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()))
			http.Error(w, "rate-limited; try later", http.StatusTooManyRequests)
			log.Printf("[%s] WAIT %s: exceeded max -> 429", reqID, r.URL.Path)
			return false
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
			log.Printf("[%s] WAIT %s: done waiting %v", reqID, r.URL.Path, wait.Truncate(time.Millisecond))
			return true
		case <-r.Context().Done():
			w.WriteHeader(statusClientClosed)
			_, _ = w.Write([]byte("client cancelled"))
			log.Printf("[%s] WAIT %s: client cancelled", reqID, r.URL.Path)
			return false
		}
	}
	return true
}

// copy with Set (avoid dup Content-Length/TE). Set-Cookie is appended.
func copyHeadersSet(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for k, vv := range src {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if strings.EqualFold(ck, "Set-Cookie") {
			for _, v := range vv {
				dst.Add(ck, v)
			}
			continue
		}
		if len(vv) > 0 {
			dst.Set(ck, vv[len(vv)-1])
		}
	}
}

func stripHopByHop(h http.Header) {
	h.Del("Connection")
	h.Del("Proxy-Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Authenticate")
	h.Del("Proxy-Authorization")
	h.Del("Te")
	h.Del("Trailer")
	h.Del("Transfer-Encoding")
	h.Del("Upgrade")
}

func lower(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

func attachTrace(reqID string, req *http.Request) {
	trace := &httptrace.ClientTrace{
		GetConn: func(hostPort string) { log.Printf("[%s] TRACE get-conn %s", reqID, hostPort) },
		GotConn: func(info httptrace.GotConnInfo) {
			log.Printf("[%s] TRACE got-conn reused=%v wasIdle=%v idleTime=%v", reqID, info.Reused, info.WasIdle, info.IdleTime)
		},
		DNSStart: func(i httptrace.DNSStartInfo) { log.Printf("[%s] TRACE dns-start host=%s", reqID, i.Host) },
		DNSDone: func(i httptrace.DNSDoneInfo) {
			log.Printf("[%s] TRACE dns-done addrs=%v err=%v", reqID, i.Addrs, i.Err)
		},
		ConnectStart:      func(n, a string) { log.Printf("[%s] TRACE conn-start %s %s", reqID, n, a) },
		ConnectDone:       func(n, a string, err error) { log.Printf("[%s] TRACE conn-done %s %s err=%v", reqID, n, a, err) },
		TLSHandshakeStart: func() { log.Printf("[%s] TRACE tls-start", reqID) },
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			log.Printf("[%s] TRACE tls-done vers=%x cipher=%x err=%v", reqID, cs.Version, cs.CipherSuite, err)
		},
		WroteHeaders:         func() { log.Printf("[%s] TRACE wrote-headers", reqID) },
		WroteRequest:         func(i httptrace.WroteRequestInfo) { log.Printf("[%s] TRACE wrote-request err=%v", reqID, i.Err) },
		GotFirstResponseByte: func() { log.Printf("[%s] TRACE first-byte", reqID) },
	}
	*req = *req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

func shortID() string {
	h := fmt.Sprintf("%x", time.Now().UnixNano())
	if len(h) > 8 {
		return h[len(h)-8:]
	}
	return h
}
