package regmetrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A realistic /debug/vars body from registry:2 in proxy mode with the debug
// endpoint enabled — trimmed to the parts we read plus the usual expvar noise,
// to prove the parser navigates registry.proxy correctly and ignores the rest.
const sampleExpvar = `{
  "cmdline": ["/bin/registry","serve","/etc/docker/registry/config.yml"],
  "memstats": {"Alloc": 1234567, "TotalAlloc": 7654321},
  "registry": {
    "proxy": {
      "Requests": 40,
      "Hits": 30,
      "Misses": 10,
      "BytesPulled": 123456789,
      "BytesPushed": 987654321
    }
  }
}`

func TestParse_ProxyStats(t *testing.T) {
	total, hits, ok, err := parse(strings.NewReader(sampleExpvar))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	if total != 40 || hits != 30 { // total = Hits+Misses = 30+10
		t.Fatalf("got total=%d hits=%d, want 40/30", total, hits)
	}
}

func TestParse_NoProxySection(t *testing.T) {
	// Debug endpoint on, but this isn't a proxy registry (no registry.proxy).
	_, _, ok, err := parse(strings.NewReader(`{"registry":{},"memstats":{}}`))
	if err != nil || ok {
		t.Fatalf("want ok=false with no error, got ok=%v err=%v", ok, err)
	}
	_, _, ok2, _ := parse(strings.NewReader(`{"memstats":{}}`))
	if ok2 {
		t.Fatalf("want ok=false when registry key absent")
	}
}

func TestParse_PresentButZero(t *testing.T) {
	// A freshly started cache that has served nothing yet is a valid ok=true
	// state with total=0 (the backend treats a 0 denominator as "no rate yet").
	total, hits, ok, err := parse(strings.NewReader(`{"registry":{"proxy":{"Requests":0,"Hits":0,"Misses":0}}}`))
	if err != nil || !ok || total != 0 || hits != 0 {
		t.Fatalf("present-but-zero: total=%d hits=%d ok=%v err=%v", total, hits, ok, err)
	}
}

func TestScrape_EndToEndOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleExpvar))
	}))
	defer srv.Close()
	total, hits, ok := Scrape(context.Background(), srv.URL+"/debug/vars")
	if !ok || total != 40 || hits != 30 {
		t.Fatalf("scrape: total=%d hits=%d ok=%v", total, hits, ok)
	}

	// Empty URL and unreachable host both mean "no metric", not a crash.
	if _, _, ok := Scrape(context.Background(), ""); ok {
		t.Fatal("empty URL should be ok=false")
	}
	if _, _, ok := Scrape(context.Background(), "http://127.0.0.1:1/debug/vars"); ok {
		t.Fatal("unreachable should be ok=false")
	}
}
