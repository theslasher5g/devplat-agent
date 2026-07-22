// Package regmetrics scrapes the per-host pull-through registry cache's
// proxy statistics so the control plane can show a real image-cache hit rate
// (instead of the dashboard's long-standing placeholder).
//
// The cache is Docker's `registry:2` in proxy mode (see
// deploy/docker-compose.registry-cache.yml). With its debug endpoint enabled
// (REGISTRY_HTTP_DEBUG_ADDR), it publishes Go expvar at /debug/vars including
// the proxy scheduler's cumulative counters under registry.proxy:
//
//	{ "registry": { "proxy": { "Requests": N, "Hits": N, "Misses": N, ... } } }
//
// Hits are layers/manifests served from local cache; Misses are fetched from
// the upstream registry. We report Hits and total lookups (Hits+Misses) as
// raw cumulative counters so the backend can pool them correctly across hosts
// (summing counters, not averaging per-host rates).
package regmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type proxyStats struct {
	Requests uint64 `json:"Requests"`
	Hits     uint64 `json:"Hits"`
	Misses   uint64 `json:"Misses"`
}

// expvarPayload uses pointers so we can tell "the proxy section is absent"
// (debug endpoint off, or not a proxy registry → ok=false) apart from "present
// but all zero" (a freshly started cache that has served nothing yet → ok=true
// with total=0).
type expvarPayload struct {
	Registry *struct {
		Proxy *proxyStats `json:"proxy"`
	} `json:"registry"`
}

// Scrape fetches the registry cache's expvar and returns cumulative
// (totalLookups, hits). ok is false when the URL is empty, unreachable, or the
// response has no registry.proxy section — in which case the caller reports no
// cache metric rather than a misleading zero. total is Hits+Misses so the
// derived hit rate is well-defined regardless of how the registry counts its
// own "Requests".
func Scrape(ctx context.Context, url string) (total, hits uint64, ok bool) {
	if url == "" {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, false
	}
	total, hits, ok, err = parse(resp.Body)
	if err != nil {
		return 0, 0, false
	}
	return total, hits, ok
}

// parse is separated from the HTTP plumbing so it can be unit-tested against a
// captured expvar sample without a running registry.
func parse(r interface{ Read([]byte) (int, error) }) (total, hits uint64, ok bool, err error) {
	var p expvarPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return 0, 0, false, fmt.Errorf("decode expvar: %w", err)
	}
	if p.Registry == nil || p.Registry.Proxy == nil {
		return 0, 0, false, nil // debug endpoint present but no proxy stats
	}
	ps := p.Registry.Proxy
	return ps.Hits + ps.Misses, ps.Hits, true, nil
}
