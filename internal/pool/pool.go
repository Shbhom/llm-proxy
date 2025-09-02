package pool

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Shbhom/llm-proxy/internal/types"
)

// type Pool struct {
// 	mu     sync.Mutex
// 	keys   []*types.Key
// 	state  map[string]*types.KeyState
// 	st     types.Store
// 	cfg    *types.Config
// 	stopCh chan struct{}
// }

type Pool struct {
	cfg   *types.Config
	st    types.Store              // <-- keep a ref to Bolt
	ready map[string]*atomic.Int64 // keyID -> nextReadyUnix (ns); 0 means "now"
	keys  []*types.Key
	byID  map[string]*types.Key
	rr    atomic.Uint64
}

func New(cfg *types.Config, st types.Store) (*Pool, error) {
	if len(cfg.Keys) == 0 {
		return nil, errors.New("no keys configured")
	}

	p := &Pool{
		cfg:  cfg,
		st:   st,
		byID: make(map[string]*types.Key),
	}

	// materialize keys
	for _, k := range cfg.Keys {
		if !k.Enabled {
			continue
		}
		id := sha1sum(k.Value) // stable deterministic ID per secret
		p.keys = append(p.keys, &types.Key{
			ID: id, Name: k.Name, Value: k.Value, Weight: max(1, k.Weight), Enabled: k.Enabled,
		})
	}

	if st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if m, err := st.LoadAllReady(ctx); err == nil {
			for id, t := range m {
				// if k := p.byID[id]; k != nil {
				// 	k.nextReadyUnix.Store(t.UnixNano())
				// }
				if a, ok := p.ready[id]; ok {
					a.Store(t.UnixNano())
				}
			}
		} else {
			log.Printf("[pool] load ready times error: %v", err)
		}
	}
	return p, nil
}

func (p *Pool) Stop() {}

// Select chooses a key that can serve now or returns earliest ready time.
// needTokens can be 0 (unknown). Caller may wait up to maxWait.
// func (p *Pool) Select(needTokens int) (key *types.Key, readyAt time.Time) {
// 	p.mu.Lock()
// 	defer p.mu.Unlock()

// 	now := time.Now()
// 	avail := make([]*types.Key, 0, len(p.keys))
// 	swr := util.NewSWRR[*types.Key]()

// 	for _, k := range p.keys {
// 		s := p.state[k.ID]
// 		if !k.Enabled || !s.Enabled || now.Before(s.DisabledUntil) {
// 			continue
// 		}
// 		if !exhausted(s.MinReq) && !exhaustedDay(s, needTokens) && (needTokens == 0 || s.MinTok.Remaining >= needTokens || s.MinTok.Limit == 0) {
// 			avail = append(avail, k)
// 			swr.Add(k, k.Weight)
// 		}
// 	}

// 	if len(avail) > 0 {
// 		k, _ := swr.Next()
// 		return k, now
// 	}

// 	// none available → find earliest ready
// 	type cand struct {
// 		k  *types.Key
// 		at time.Time
// 	}
// 	var cs []cand
// 	for _, k := range p.keys {
// 		s := p.state[k.ID]
// 		at := earliestReady(s, needTokens)
// 		cs = append(cs, cand{k, at})
// 	}
// 	sort.Slice(cs, func(i, j int) bool { return cs[i].at.Before(cs[j].at) })
// 	return cs[0].k, cs[0].at
// }

func (p *Pool) Select(_ int) (*types.Key, time.Time) {
	n := len(p.keys)
	if n == 0 {
		// return a dummy key that will fail fast upstream
		return &types.Key{ID: "none", Name: "none", Value: ""}, time.Now()
	}
	i := int(p.rr.Add(1)-1) % n
	k := p.keys[i]

	rd := p.ready[k.ID]
	var t time.Time
	if rd != nil {
		ns := rd.Load()
		if ns != 0 {
			t = time.Unix(0, ns)
		}
	}
	if t.IsZero() {
		t = time.Now()
	}

	// Debug line to see selection/wait quickly.
	wait := time.Until(t).Truncate(time.Millisecond)
	log.Printf("[pool] select -> %s readyIn=%v", k.Name, wait)
	return k, t
}

// func (p *Pool) ProvisionalConsume(keyID string, needTokens int) {
// 	p.mu.Lock()
// 	defer p.mu.Unlock()
// 	s := p.state[keyID]
// 	// minute windows roll every minute; conservative decrement
// 	if s.MinReq.Limit > 0 && s.MinReq.Remaining > 0 {
// 		s.MinReq.Remaining--
// 	}
// 	if needTokens > 0 && s.MinTok.Limit > 0 && s.MinTok.Remaining >= needTokens {
// 		s.MinTok.Remaining -= needTokens
// 	}
// }

func (p *Pool) ProvisionalConsume(_ string, _ int) {}

// func (p *Pool) ReconcileFromHeaders(keyID string, hdr map[string]string, status int) {
// 	p.mu.Lock()
// 	defer p.mu.Unlock()
// 	s := p.state[keyID]
// 	now := time.Now()

// 	// helper to parse ints & durations
// 	parseInt := func(k string) (int, bool) {
// 		if v, ok := hdr[k]; ok {
// 			var x int
// 			_, err := fmtSscanf(v, &x)
// 			if err == nil {
// 				return x, true
// 			}
// 		}
// 		return 0, false
// 	}
// 	parseDur := func(k string) (time.Duration, bool) {
// 		if v, ok := hdr[k]; ok {
// 			d, err := time.ParseDuration(v)
// 			if err == nil {
// 				return d, true
// 			}
// 			// sometimes it's "6s" or milliseconds; fall back to seconds integer
// 			if n, e := atoiSafe(v); e == nil {
// 				return time.Duration(n) * time.Second, true
// 			}
// 		}
// 		return 0, false
// 	}

// 	if lim, ok := parseInt("x-ratelimit-limit-requests"); ok {
// 		s.MinReq.Limit = lim
// 	}
// 	if rem, ok := parseInt("x-ratelimit-remaining-requests"); ok {
// 		s.MinReq.Remaining = rem
// 	}
// 	if dur, ok := parseDur("x-ratelimit-reset-requests"); ok {
// 		s.MinReq.ResetAt = now.Add(dur)
// 	}

// 	if lim, ok := parseInt("x-ratelimit-limit-tokens"); ok {
// 		s.MinTok.Limit = lim
// 	}
// 	if rem, ok := parseInt("x-ratelimit-remaining-tokens"); ok {
// 		s.MinTok.Remaining = rem
// 	}
// 	if dur, ok := parseDur("x-ratelimit-reset-tokens"); ok {
// 		s.MinTok.ResetAt = now.Add(dur)
// 	}

// 	// day windows may be absent; if present, treat similarly (optional)

// 	// 429 handling via retry-after
// 	if status == 429 {
// 		if dur, ok := parseDur("retry-after"); ok {
// 			s.DisabledUntil = now.Add(dur)
// 		}
// 		// set remainings pessimistically
// 		if s.MinReq.Limit > 0 {
// 			s.MinReq.Remaining = 0
// 		}
// 		if s.MinTok.Limit > 0 {
// 			s.MinTok.Remaining = 0
// 		}
// 	}

// 	_ = p.st.Upsert(context.Background(), s)
// }

func (p *Pool) ReconcileFromHeaders(keyID string, hdr map[string]string, status int) {
	k := p.byID[keyID]
	if k == nil {
		return
	}
	now := time.Now()

	reqRemain := atoiSafe(hdr["x-ratelimit-remaining-requests"])
	// tokRemain := atoiSafe(hdr["x-ratelimit-remaining-tokens"])//optional
	reqReset := parseGroqDur(hdr["x-ratelimit-reset-requests"])
	// tokReset := parseGroqDur(hdr["x-ratelimit-reset-tokens"])//optional

	next := now
	if status == 429 || reqRemain == 0 {
		if reqReset > 0 && now.Add(reqReset).After(next) {
			next = now.Add(reqReset)
		}
	}
	// // Optional: if you want token-based backoff too
	// if tokRemain == 0 && tokReset > 0 && now.Add(tokReset).After(next) {
	// 	next = now.Add(tokReset)
	// }

	// Update in-memory readiness atomically.
	if a := p.ready[keyID]; a != nil {
		if next.After(now) {
			a.Store(next.UnixNano())
		} else {
			a.Store(0)
		}
	}

	// Persist asynchronously (never block the handler).
	if p.st != nil {
		p.st.UpsertReadyAtAsync(keyID, next)
	}

}

// helpers

func exhausted(w types.Window) bool {
	return w.Limit > 0 && w.Remaining <= 0 && time.Now().Before(w.ResetAt)
}
func exhaustedDay(s *types.KeyState, needTokens int) bool {
	if s.DayReq.Limit > 0 && s.DayReq.Remaining <= 0 && time.Now().Before(s.DayReq.ResetAt) {
		return true
	}
	if needTokens > 0 && s.DayTok.Limit > 0 && s.DayTok.Remaining < needTokens && time.Now().Before(s.DayTok.ResetAt) {
		return true
	}
	return false
}
func earliestReady(s *types.KeyState, needTokens int) time.Time {
	cands := []time.Time{}
	if exhausted(s.MinReq) {
		cands = append(cands, s.MinReq.ResetAt)
	}
	if needTokens > 0 && s.MinTok.Limit > 0 && s.MinTok.Remaining < needTokens {
		cands = append(cands, s.MinTok.ResetAt)
	}
	if s.DayReq.Limit > 0 && s.DayReq.Remaining <= 0 {
		cands = append(cands, s.DayReq.ResetAt)
	}
	if needTokens > 0 && s.DayTok.Limit > 0 && s.DayTok.Remaining < needTokens {
		cands = append(cands, s.DayTok.ResetAt)
	}
	if time.Now().Before(s.DisabledUntil) {
		cands = append(cands, s.DisabledUntil)
	}
	if len(cands) == 0 {
		return time.Now()
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Before(cands[j]) })
	return cands[0]
}

func sha1sum(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// tiny safe parsers
//
//	func atoiSafe(s string) (int, error) {
//		var n int
//		_, err := fmtSscanf(strings.TrimSpace(s), &n)
//		return n, err
//	}
func fmtSscanf(v string, out *int) (int, error) {
	// very small shim avoiding fmt import in this file’s header list churn
	// (we’ll just use fmt.Sscanf under the hood)
	return fmt.Scanf(v, out)
}

func atoiSafe(s string) int {
	if s == "" {
		return -1
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return v
}

func parseGroqDur(s string) time.Duration {
	// Groq returns strings like "2m1.579999999s" / "30ms" / "0s"
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
}
