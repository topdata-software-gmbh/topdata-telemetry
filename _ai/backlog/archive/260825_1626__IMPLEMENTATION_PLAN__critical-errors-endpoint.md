---
filename: "_ai/backlog/active/260825_1626__IMPLEMENTATION_PLAN__critical-errors-endpoint.md"
title: "New /critical-errors endpoint: recent critical errors per shop"
createdAt: 2026-08-25 16:26
updatedAt: 2026-08-25 16:53
status: completed
completedAt: 2026-08-25 16:54
priority: medium
tags: [golang, http-api, monitoring, observability]
estimatedComplexity: simple
documentRevision: 1
documentType: IMPLEMENTATION_PLAN
id: 7631ec44-41d5-47b3-af84-86627255ed23
content_hash: 3cdc530aefa66ad9671c3704ba10784a
---

# Implementation Plan: `/critical-errors` endpoint

## 1. Problem

Today the agent only **counts** critical Shopware log lines: `TailLog`
(`internal/monitor/log_monitor.go`) matches `.CRITICAL:` / `[critical]` and
increments the `topdata_agent_shopware_critical_errors_total` counter. The
actual error text is discarded. When an operator (or an AI agent doing
debugging/fixing) wants to know *what* recently broke on which shop, they must
manually SSH into the server and grep the daily log by hand. There is no way to
ask the running agent "show me the latest critical errors, optionally for shop
X".

## 2. Executive Summary

Add a Basic-Auth-protected HTTP endpoint `GET /critical-errors` that reports
the most recent critical error lines per shop, backed by a small in-memory
ring buffer (fixed 100 lines per shop, **full untruncated lines** — the primary
consumer is an AI agent that needs complete messages for debugging). The
existing `TailLog` goroutine feeds the buffer at the same place where it
already increments the counter — zero extra I/O, no new dependencies. The
endpoint supports `?shop=<name>` filtering, `?limit=N`, and the established
`?format=json|text|markdown` negotiation (with `Accept`-header fallback) used
by `/disk-eaters` and `/info`. Buffer history intentionally resets on agent
restart (the payload carries `agent_started_at` to make that explicit);
removed shops are purged exactly like their Prometheus series are today.

Design decisions were recorded in brainstorm
`latest-critical-errors-endpoint` (control-plane id `50ee67e6-b3ce-4cb9-8d03-3b3288947934`):

| Decision | Choice |
|---|---|
| Payload | Recent history (last N lines per shop), not just latest/timestamps |
| Data source | In-memory ring buffer fed by the existing tail goroutine |
| Capacity | Fixed 100 lines/shop, hardcoded (no new config key) |
| Midnight rotation | Keep rolling across day boundaries; entries carry timestamps |
| Endpoint | `/critical-errors` with `?shop=`, `?limit=`, `?format=` |

## 3. Project Environment

- Project Name: **topdata-node-agent-v2** (Go CLI App)
- Repo path: `/topdata/topdata-node-agent-v2`
- Go 1.21+, module `github.com/topdata/node-agent`
- Entry point: `main.go` → Cobra CLI (`cmd/root.go`, `cmd/serve.go`); `serve` is the only subcommand
- Key packages: `internal/discovery` (shop discovery), `internal/monitor` (supervisor, log tail, disk scanner)
- Dependencies: `spf13/cobra`, `spf13/viper`, `prometheus/client_golang`, `hpcloud/tail` (+ required `fsnotify` replace in `go.mod` — **do not remove**)
- Build/verify: `go build ./... && go vet ./...` (no Makefile, no CI)
- Tests: repo currently has **no** test files; `go test ./...` is valid but empty. This plan adds the first unit-test file (safe: no new `promauto` registrations are triggered by tests — the warning about duplicate registration only applies to running `serve` twice).
- Deployment: Ansible playbook in `deploy/`; releases via `scripts/deploy/deploy-next-version.sh` rotating `CHANGELOG.md`
- Conventions: `_ai/` knowledge base; ADRs in `_ai/technical_decisions/`; plans in `_ai/backlog/active/`; reports in `_ai/backlog/reports/`
- Implemented by an AI coding agent.

## 4. Design

### 4.1 Components

```
TailLog goroutine (existing)          HTTP layer
┌────────────────────────────┐        ┌──────────────────────────────────┐
│ match .CRITICAL:/[critical]│───►    │ recordCritical(shop, line)       │
│ criticalErrors…Inc()       │        │ criticalStore: map[shop][]entry  │
└────────────────────────────┘        │ (mutex, cap 100/shop)            │
                                      └───────────────┬──────────────────┘
RemoveShopLog (existing)              snapshotCritical(shop, limit)
      │                               ┌───────────────▼──────────────────┐
      └──► removeShopCritical(shop)   │ CriticalErrorsHandler(startedAt) │
                                      │ /critical-errors (authMiddleware)│
                                      └──────────────────────────────────┘
```

Single responsibility split (SOLID):
- **Buffer** (`error_buffer.go`) owns storage/eviction — knows nothing about HTTP.
- **TailLog** keeps its single job: tail + count, plus one `recordCritical` call.
- **Handler** owns parsing/negotiation/rendering — reads via snapshot copies only.

### 4.2 API contract

`GET /critical-errors` — Basic Auth (same `authMiddleware` as `/metrics`).

| Param | Default | Behaviour |
|---|---|---|
| `shop` | *(all shops)* | Only that shop's entries; unknown/removed shop → empty result, **never 404** |
| `limit` | `20` | Max entries per shop; values `> 100` clamped to 100 (buffer size); invalid values fall back to default |
| `format` | Accept-header fallback (`text/markdown` → markdown, `text/plain` → text, else JSON) | Same negotiation as `/disk-eaters` and `/info` |

JSON shape (newest first within each shop):

```json
{
  "agent_started_at": "2026-08-25T14:00:00Z",
  "shops": {
    "muster-shop": [
      { "timestamp": "2026-08-25T16:03:11Z",
        "message": "app.ERROR: request 9f2c .CRITICAL: class not found ..." }
    ]
  },
  "total": 1
}
```

Text: one block per shop (`SHOP (n entries)` header + `TIMESTAMP MESSAGE` rows).
Markdown: `## <shop> (n entries)` heading + pipe table; `|` inside messages is escaped.
Empty state (both text formats): `no critical errors recorded since agent start (<rfc3339>)`.

### 4.3 Semantics

- **Full fidelity**: lines are stored verbatim, never truncated (AI-agent requirement).
- **Capacity**: 100 entries/shop ⇒ worst case ~10 shops × 100 × typical line size ≈ low MBs. Bounded and predictable.
- **Midnight rotation**: the buffer keeps rolling; old entries remain until evicted. No clearing.
- **Restart**: buffer starts empty; `agent_started_at` distinguishes "all quiet" from "buffer cold".
- **Removed shops**: `RemoveShopLog` also purges buffer entries (mirrors series deletion).
- **Concurrency**: one `sync.Mutex`; writers = tail goroutines (rare appends); readers copy under lock and render outside it.

---

## Phase 1 — Core ring buffer + unit tests

### 1a. [NEW FILE] `internal/monitor/error_buffer.go`

```go
package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// criticalBufferSize caps how many critical-error lines are kept per shop.
	// Fixed by design (brainstorm decision): no config knob, bounded memory.
	criticalBufferSize = 100
	// criticalDefaultLimit is the per-shop entry count returned by
	// /critical-errors when ?limit= is absent.
	criticalDefaultLimit = 20
)

// CriticalEntry is one captured critical Shopware log line. The message is
// stored verbatim (never truncated) so consumers — notably AI agents — get the
// full error text needed for debugging.
type CriticalEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

// The critical-line store mirrors the package-level Prometheus metrics in
// log_monitor.go: a single mutex-guarded global shared by all per-shop tail
// goroutines (writers) and the /critical-errors handler (reader).
var (
	criticalMu    sync.Mutex
	criticalStore = map[string][]CriticalEntry{} // shop name -> oldest..newest
)

// recordCritical appends a critical line for a shop, evicting the oldest
// entry once the per-shop buffer exceeds criticalBufferSize.
func recordCritical(shop, line string) {
	entry := CriticalEntry{Timestamp: time.Now(), Message: line}
	criticalMu.Lock()
	defer criticalMu.Unlock()
	buf := append(criticalStore[shop], entry)
	if len(buf) > criticalBufferSize {
		buf = buf[len(buf)-criticalBufferSize:]
	}
	criticalStore[shop] = buf
}

// removeShopCritical drops all buffered entries for a removed shop so
// /critical-errors stops reporting it, mirroring the deletion of the shop's
// Prometheus series in RemoveShopLog.
func removeShopCritical(shop string) {
	criticalMu.Lock()
	defer criticalMu.Unlock()
	delete(criticalStore, shop)
}

// snapshotCritical returns a copy of the buffered entries, newest first,
// optionally scoped to one shop, at most limit entries per shop (limit <= 0
// means unlimited). Callers own the returned value; the lock is released
// before any rendering happens.
func snapshotCritical(shop string, limit int) map[string][]CriticalEntry {
	criticalMu.Lock()
	defer criticalMu.Unlock()

	out := make(map[string][]CriticalEntry, len(criticalStore))
	for name, buf := range criticalStore {
		if shop != "" && name != shop {
			continue
		}
		entries := make([]CriticalEntry, len(buf))
		copy(entries, buf)
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
		if limit > 0 && len(entries) > limit {
			entries = entries[:limit]
		}
		out[name] = entries
	}
	return out
}

// criticalPayload is the JSON document served by /critical-errors.
type criticalPayload struct {
	AgentStartedAt string                     `json:"agent_started_at"`
	Shops          map[string][]CriticalEntry `json:"shops"`
	Total          int                        `json:"total"`
}

// CriticalErrorsHandler serves the /critical-errors listing behind basic auth.
//
// Query parameters:
//   - ?shop=<name>  only that shop's entries (unknown shop -> empty list)
//   - ?limit=<n>    max entries per shop (default 20, capped at buffer size)
//
// Output format is chosen by `?format=json|text|markdown`; if absent it falls
// back to content negotiation via the Accept header (text/plain or
// text/markdown) and finally defaults to JSON — the same negotiation as
// /disk-eaters and /info.
func CriticalErrorsHandler(agentStartedAt time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shop := r.URL.Query().Get("shop")
		limit := criticalDefaultLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > criticalBufferSize {
			limit = criticalBufferSize
		}
		entries := snapshotCritical(shop, limit)

		format := r.URL.Query().Get("format")
		if format == "" {
			accept := r.Header.Get("Accept")
			switch {
			case strings.Contains(accept, "text/markdown"):
				format = "markdown"
			case strings.Contains(accept, "text/plain"):
				format = "text"
			default:
				format = "json"
			}
		}

		switch format {
		case "text":
			writeCriticalText(w, entries, agentStartedAt)
		case "markdown":
			writeCriticalMarkdown(w, entries, agentStartedAt)
		default:
			total := 0
			for _, list := range entries {
				total += len(list)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(criticalPayload{
				AgentStartedAt: agentStartedAt.Format(time.RFC3339),
				Shops:          entries,
				Total:          total,
			})
		}
	})
}

// sortedCriticalShops returns the shop names in a snapshot alphabetically for
// deterministic text/markdown output.
func sortedCriticalShops(entries map[string][]CriticalEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// writeCriticalText renders the plain-text listing: one labelled block per
// shop with TIMESTAMP/MESSAGE columns.
func writeCriticalText(w http.ResponseWriter, entries map[string][]CriticalEntry, startedAt time.Time) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(entries) == 0 {
		fmt.Fprintf(w, "no critical errors recorded since agent start (%s)\n", startedAt.Format(time.RFC3339))
		return
	}
	for _, shop := range sortedCriticalShops(entries) {
		fmt.Fprintf(w, "%s (%d entries)\n", shop, len(entries[shop]))
		fmt.Fprintf(w, "%-25s %s\n", "TIMESTAMP", "MESSAGE")
		for _, e := range entries[shop] {
			fmt.Fprintf(w, "%-25s %s\n", e.Timestamp.Format(time.RFC3339), e.Message)
		}
		fmt.Fprintln(w)
	}
}

// writeCriticalMarkdown renders the markdown listing: one heading + table per
// shop. Pipes inside messages are escaped to keep the table intact.
func writeCriticalMarkdown(w http.ResponseWriter, entries map[string][]CriticalEntry, startedAt time.Time) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	if len(entries) == 0 {
		fmt.Fprintf(w, "_no critical errors recorded since agent start (%s)_\n", startedAt.Format(time.RFC3339))
		return
	}
	for _, shop := range sortedCriticalShops(entries) {
		fmt.Fprintf(w, "## %s (%d entries)\n\n", shop, len(entries[shop]))
		fmt.Fprintf(w, "| Timestamp | Message |\n")
		fmt.Fprintf(w, "| --- | --- |\n")
		for _, e := range entries[shop] {
			msg := strings.ReplaceAll(e.Message, "|", "\\|")
			fmt.Fprintf(w, "| %s | %s |\n", e.Timestamp.Format(time.RFC3339), msg)
		}
		fmt.Fprintln(w)
	}
}
```

Notes:
- No comments beyond doc comments, per repo style (existing files carry doc comments only).
- Imports ordered stdlib-only here (single group).

### 1b. [NEW FILE] `internal/monitor/error_buffer_test.go`

First test file in the repo (conventions: table-driven where sensible). Safe
with respect to the global `promauto` registry — the test never starts `serve`.

```go
package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// resetCriticalStore clears the global buffer between tests. Tests must not
// run in parallel (shared global state).
func resetCriticalStore() {
	criticalMu.Lock()
	defer criticalMu.Unlock()
	criticalStore = map[string][]CriticalEntry{}
}

func TestRecordCriticalEvictsOldest(t *testing.T) {
	resetCriticalStore()
	for i := 0; i < criticalBufferSize+10; i++ {
		recordCritical("shop-a", fmt.Sprintf("line-%d", i))
	}
	got := snapshotCritical("shop-a", 0)
	if len(got["shop-a"]) != criticalBufferSize {
		t.Fatalf("buffer size = %d, want %d", len(got["shop-a"]), criticalBufferSize)
	}
	first := got["shop-a"][0]
	last := got["shop-a"][len(got["shop-a"])-1]
	if first.Message != "line-10" {
		t.Errorf("oldest kept = %q, want line-10", first.Message)
	}
	if last.Message != fmt.Sprintf("line-%d", criticalBufferSize+9) {
		t.Errorf("newest = %q, want line-%d", last.Message, criticalBufferSize+9)
	}
}

func TestSnapshotCriticalNewestFirstAndLimit(t *testing.T) {
	resetCriticalStore()
	base := time.Now()
	criticalMu.Lock()
	for i := 0; i < 5; i++ {
		e := CriticalEntry{Timestamp: base.Add(time.Duration(i) * time.Second), Message: fmt.Sprintf("m%d", i)}
		criticalStore["shop-a"] = append(criticalStore["shop-a"], e)
	}
	criticalMu.Unlock()

	got := snapshotCritical("", 3)
	if got["shop-a"][0].Message != "m4" || got["shop-a"][2].Message != "m2" {
		t.Errorf("snapshot not newest-first with limit applied: %+v", got["shop-a"])
	}
	if len(got["shop-a"]) != 3 {
		t.Fatalf("len = %d, want 3", len(got["shop-a"]))
	}

	full := snapshotCritical("", 0)
	if len(full["shop-a"]) != 5 {
		t.Errorf("unlimited snapshot len = %d, want 5", len(full["shop-a"]))
	}
}

func TestSnapshotCriticalShopFilterIsolation(t *testing.T) {
	resetCriticalStore()
	recordCritical("shop-a", "a1")
	recordCritical("shop-b", "b1")

	onlyA := snapshotCritical("shop-a", 0)
	if _, ok := onlyA["shop-b"]; ok {
		t.Errorf("?shop= filter leaked other shops: %+v", onlyA)
	}
	if len(onlyA["shop-a"]) != 1 || onlyA["shop-a"][0].Message != "a1" {
		t.Errorf("wrong entries for shop-a: %+v", onlyA["shop-a"])
	}

	all := snapshotCritical("", 0)
	if len(all) != 2 {
		t.Errorf("unfiltered snapshot has %d shops, want 2", len(all))
	}

	unknown := snapshotCritical("does-not-exist", 0)
	if len(unknown) != 0 {
		t.Errorf("unknown shop must yield empty result, got %+v", unknown)
	}
}

func TestRemoveShopCritical(t *testing.T) {
	resetCriticalStore()
	recordCritical("shop-a", "a1")
	removeShopCritical("shop-a")
	got := snapshotCritical("shop-a", 0)
	if len(got) != 0 {
		t.Errorf("removed shop still present: %+v", got)
	}
}

func TestCriticalErrorsHandlerFormats(t *testing.T) {
	resetCriticalStore()
	started := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	recordCritical("shop-a", "boom .CRITICAL: x|y")

	h := CriticalErrorsHandler(started)

	t.Run("json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/critical-errors?shop=shop-a", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		var p criticalPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if p.AgentStartedAt != started.Format(time.RFC3339) {
			t.Errorf("agent_started_at = %q", p.AgentStartedAt)
		}
		if p.Total != 1 || len(p.Shops["shop-a"]) != 1 || p.Shops["shop-a"][0].Message != "boom .CRITICAL: x|y" {
			t.Errorf("payload mismatch: %+v", p)
		}
	})

	t.Run("text", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/critical-errors?format=text", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if want := "no critical errors recorded"; false {
			_ = want // placeholder, replaced below
		}
		for _, want := range []string{"shop-a (1 entries)", "MESSAGE", "boom .CRITICAL: x|y"} {
			if !contains(body, want) {
				t.Errorf("text output missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("markdown escapes pipes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/critical-errors?format=markdown", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		for _, want := range []string{"## shop-a (1 entries)", `x\|y`} {
			if !contains(body, want) {
				t.Errorf("markdown output missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("empty state", func(t *testing.T) {
		resetCriticalStore()
		req := httptest.NewRequest(http.MethodGet, "/critical-errors?shop=nobody", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if body := rec.Body.String(); !contains(body, "no critical errors recorded since agent start") {
			t.Errorf("missing empty-state text:\n%s", body)
		}
	})

	t.Run("limit clamp", func(t *testing.T) {
		resetCriticalStore()
		for i := 0; i < 30; i++ {
			recordCritical("shop-a", fmt.Sprintf("m%d", i))
		}
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/critical-errors?limit=%d&shop=shop-a", criticalBufferSize+50), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var p criticalPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if len(p.Shops["shop-a"]) != criticalBufferSize {
			t.Errorf("clamped len = %d, want %d", len(p.Shops["shop-a"]), criticalBufferSize)
		}
	})
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
```

> Implementation note for the coding agent: `strings.Contains` is fine here —
> replace the hand-rolled `contains`/`indexOf` helpers with
> `strings.Contains(body, want)` if preferred (add `"strings"` to imports).
> The dead `if want := ...` placeholder in the `text` subtest must be deleted.

Validation gate for Phase 1:

```sh
gofmt -l .
go build ./...
go vet ./...
go test ./internal/monitor/ -run 'Critical' -v
```

---

## Phase 2 — Capture wiring in the log tail

### 2a. [MODIFY] `internal/monitor/log_monitor.go`

Two surgical edits; tail/watchdog logic untouched (per AGENTS.md guardrails).

Edit 1 — record alongside the counter increment (consumer goroutine):

```go
			go func(t *tail.Tail) {
				for line := range t.Lines {
					if strings.Contains(line.Text, ".CRITICAL:") || strings.Contains(line.Text, "[critical]") {
						recordCritical(shopName, line.Text)
						criticalErrors.WithLabelValues(shopName).Inc()
					}
				}
			}(current)
```

Edit 2 — purge buffer on shop removal:

```go
// RemoveShopLog deletes the critical-errors series and buffered error lines
// for a removed shop.
func RemoveShopLog(shop string) {
	criticalErrors.DeleteLabelValues(shop)
	removeShopCritical(shop)
}
```

No import changes needed (`recordCritical` lives in the same package).

Because the store is package-level, the recreated consumer goroutine after
midnight rotation transparently continues appending — the "keep rolling"
decision needs no extra code.

---

## Phase 3 — Register the endpoint

### 3a. [MODIFY] `cmd/serve.go`

One line added to the route registration block in `Run`:

```go
		http.HandleFunc("/healthz", healthzHandler)
		http.Handle("/metrics", authMiddleware(promhttp.Handler()))
		http.Handle("/disk-eaters", authMiddleware(scanner.Handler()))
		http.Handle("/info", authMiddleware(http.HandlerFunc(infoHandler)))
		http.Handle("/critical-errors", authMiddleware(monitor.CriticalErrorsHandler(startTime)))
		log.Fatal(http.ListenAndServe(viper.GetString("listen.address"), nil))
```

`startTime` (already a package var, set at process start) is passed in rather
than read globally by the `monitor` package — keeps the dependency direction
strictly `cmd → monitor`.

No new flags, no new viper keys, no import changes.

---

## Phase 4 — Verification

1. Compile + static checks (must be clean):

   ```sh
   gofmt -l .
   go build ./...
   go vet ./...
   go test ./...
   ```

2. Manual smoke test (repo has no integration harness):

   ```sh
   go build -o bin/topdata-agent .

   SMOKE=/tmp/opencode/agent-smoke
   rm -rf "$SMOKE" && mkdir -p "$SMOKE/shops/demo-shop/vol/www/var/log"

   TOPDATA_AGENT_SHOPS_ROOT="$SMOKE/shops" \
   TOPDATA_AGENT_AUTH_USERNAME=u TOPDATA_AGENT_AUTH_PASSWORD=p \
   TOPDATA_AGENT_LISTEN_ADDRESS=:19144 \
     ./bin/topdata-agent serve &
   sleep 2   # let discovery pick up the shop and the poller attach

   LOG="$SMOKE/shops/demo-shop/vol/www/var/log/prod-$(date +%F).log"
   echo "2026-08-25T16:00:00+00:00 app.ERROR: .CRITICAL: first failure" >> "$LOG"
   echo "2026-08-25T16:01:00+00:01 foo [critical] second failure"      >> "$LOG"
   echo "2026-08-25T16:02:00+00:02 app.WARNING: not critical"          >> "$LOG"
   sleep 2   # Poll:true detection latency

   curl -su u:p 'http://localhost:19144/critical-events' 2>/dev/null # typo check: expect 404
   curl -su u:p 'http://localhost:19144/critical-errors'                    # JSON, total=2
   curl -su u:p 'http://localhost:19144/critical-errors?shop=demo-shop'     # same, filtered
   curl -su u:p 'http://localhost:19144/critical-errors?shop=nope'          # empty, 200
   curl -su u:p 'http://localhost:19144/critical-errors?limit=1&format=text'
   curl -su u:p -H 'Accept: text/markdown' 'http://localhost:19144/critical-errors'
   curl -su u:wrong 'http://localhost:19144/critical-errors'                # expect 401
   curl -s       'http://localhost:19144/critical-errors'                   # expect 401
   ```

   Expected: exactly the two critical lines appear (warning excluded), full
   messages untruncated, newest first, all three formats render, auth enforced.

3. Restart semantics spot-check: kill `serve`, restart, hit the endpoint →
   empty result with fresh `agent_started_at`.

Kill background server afterwards: `pkill -f 'bin/topdata-agent serve'`.

---

## Phase 5 — Housekeeping & documentation

### 5a. [MODIFY] `CHANGELOG.md`

The file currently has **three** stacked `## [Unreleased]` headings (an
artifact of earlier rotations). Collapse them into one and add the feature
under it:

```markdown
## [Unreleased]

### Added
- `/critical-errors` endpoint (Basic Auth, same middleware as `/metrics`): reports the most recent critical Shopware error lines per shop from an in-memory ring buffer (100 lines/shop, full untruncated messages). Supports `?shop=<name>`, `?limit=N` (default 20, capped at 100) and `?format=json|text|markdown` with the usual `Accept`-header fallback. Entries carry timestamps and survive midnight log rotation; history resets on agent restart (the payload includes `agent_started_at`). Removed shops are purged together with their Prometheus series.
- First unit tests (`internal/monitor/error_buffer_test.go`) covering buffer eviction, snapshots and endpoint rendering.
```

Keep the existing `discovery.interval` bullet inside the merged `[Unreleased]`
section.

### 5b. [MODIFY] `README.md`

Three small edits:

1. Architecture bullet list — extend the **Log monitoring** bullet:
   ```markdown
   - **Log monitoring**: tails each shop's `vol/www/var/log/prod-YYYY-MM-DD.log` in real-time
     and counts `[CRITICAL]` entries, keeping the last 100 full error lines per shop in
     memory for the `/critical-errors` endpoint. The tail automatically restarts when the
     date changes to follow the new daily log file.
   ```
2. Replace the endpoints paragraph (currently starting "The `/metrics` and
   `/disk-eaters` endpoints require Basic Auth.") with:
   ```markdown
   The `/metrics`, `/disk-eaters`, `/info` and `/critical-errors` endpoints
   require Basic Auth. `/info` and `/disk-eaters` support
   `?format=json|text|markdown` (plus `Accept` negotiation). The `/healthz`
   endpoint is **unauthenticated** and only returns `200 OK` while the process
   is listening — use it for liveness checks and the Ansible deploy smoke test.
   ```
3. Add a short section after **## Metrics**:
   ````markdown
   ## Recent critical errors

   ```sh
   curl -su USER:PASSWORD 'http://host:9144/critical-errors'                 # all shops, JSON
   curl -su USER:PASSWORD 'http://host:9144/critical-errors?shop=muster-shop'
   curl -su USER:PASSWORD 'http://host:9144/critical-errors?limit=5&format=markdown'
   ```

   Returns the last critical log lines per shop (up to 100 kept in memory, full
   untruncated messages, newest first). Useful for quickly answering "what just
   broke?" without SSH. History starts empty after an agent restart;
   `agent_started_at` in the response tells you since when.
   ````

### 5c. [MODIFY] `AGENTS.md`

Update the architecture bullet describing `serve`'s routes — change "It
registers three endpoints" to four and insert `/critical-errors` (Basic Auth;
recent critical error lines per shop from an in-memory 100-line-per-shop ring
buffer fed by the log tailer; `?shop=`, `?limit=`, `?format=json|text|markdown`
with Accept fallback) alongside the `/disk-eaters` description.

### 5d. `.gitignore` — **no change**

No new file types, directories, or build artifacts are introduced (`bin/` is
already ignored; smoke-test artifacts live in `/tmp/opencode`).

---

## Phase 6 — Implementation report

Write `_ai/backlog/reports/{YYMMDD_HHmm}__IMPLEMENTATION_REPORT__critical-errors-endpoint.md`
(timestamp filled at completion) with frontmatter:

```yaml
---
filename: "_ai/backlog/reports/{YYMMDD_HHmm}__IMPLEMENTATION_REPORT__critical-errors-endpoint.md"
title: "Report: New /critical-errors endpoint: recent critical errors per shop"
createdAt: YYYY-MM-DD HH:mm
updatedAt: YYYY-MM-DD HH:mm
planFile: "_ai/backlog/active/260825_1626__IMPLEMENTATION_PLAN__critical-errors-endpoint.md"
project: "topdata-node-agent-v2"
status: completed
completedAt: 2026-08-25 16:54
filesCreated: 2
filesModified: 4
filesDeleted: 0
tags: [golang, http-api, monitoring, observability]
documentType: IMPLEMENTATION_REPORT
---
```

Contents: summary, files changed, key changes, deviations from plan,
technical decisions, testing notes (incl. the Phase-4 smoke results), usage
examples, documentation updates, next steps. Then mark this plan's
frontmatter `status: completed` and update `updatedAt`.

Suggested next steps for the report: consider persisting the buffer across
restarts (like `disk.state_file`) if cold-start blindness becomes a problem,
and/or a follow-up ADR if the buffer ever needs to grow beyond trivial scope.

---

## Risk register

| Risk | Mitigation |
|---|---|
| Duplicate `promauto` registration panic in future tests | Tests never start `serve`; no new metrics added |
| Memory blow-up from untruncated lines | Hard cap 100/shop; pathological single lines are bounded by Shopware log line lengths |
| Lock contention | Writes are rare (critical lines only); readers copy under lock and render outside it |
| Regression in tail/watchdog behaviour | Phase 2 touches only the match branch + `RemoveShopLog`; AGENTS.md guardrails respected |
