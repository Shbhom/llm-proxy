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

func (p *Pool) ProvisionalConsume(_ string, _ int) {}

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
