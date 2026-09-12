---
filename: "_ai/backlog/reports/260825_1653__IMPLEMENTATION_REPORT__critical-errors-endpoint.md"
title: "Report: New /critical-errors endpoint: recent critical errors per shop"
createdAt: 2026-08-25 16:53
updatedAt: 2026-08-25 16:53
planFile: "_ai/backlog/active/260825_1626__IMPLEMENTATION_PLAN__critical-errors-endpoint.md"
project: "topdata-node-agent-v2"
status: completed
filesCreated: 2
filesModified: 4
filesDeleted: 0
tags: [golang, http-api, monitoring, observability]
documentType: IMPLEMENTATION_REPORT
id: 92bf9ac5-7133-475f-bfbe-f59eac7e9831
content_hash: 71aeee9ec5d70a9004d8163c867b42c3
---

# Implementation Report: `/critical-errors` endpoint

## Summary

Implemented the Basic-Auth-protected `GET /critical-errors` endpoint exactly as planned: a per-shop in-memory ring buffer (fixed 100 lines, full untruncated messages) fed by the existing log-tail goroutine at the same place where the Prometheus counter is incremented, plus an HTTP handler supporting `?shop=`, `?limit=` and `?format=json|text|markdown` with `Accept` fallback. Removed shops are purged together with their Prometheus series. All phases completed; all verification gates green including a live smoke test.

## Files changed

**Created (2)**
- `internal/monitor/error_buffer.go` — ring buffer (`recordCritical`, `removeShopCritical`, `snapshotCritical`) + `CriticalErrorsHandler` with JSON/text/markdown rendering.
- `internal/monitor/error_buffer_test.go` — first unit tests in the repo (buffer eviction, newest-first snapshots + limit, shop-filter isolation, removal purge, handler format negotiation incl. empty state and limit clamping).

**Modified (4)**
- `internal/monitor/log_monitor.go` — two surgical edits: `recordCritical(shopName, line.Text)` alongside the `criticalErrors…Inc()` call in the tail consumer goroutine; `RemoveShopLog` now also calls `removeShopCritical(shop)`. Tail/watchdog logic untouched.
- `cmd/serve.go` — one route added: `http.Handle("/critical-errors", authMiddleware(monitor.CriticalErrorsHandler(startTime)))`.
- `CHANGELOG.md` — collapsed the three stacked `[Unreleased]` headings into one; feature entry + tests entry added; existing `discovery.interval` bullet kept inside the merged section.
- `README.md` — extended the **Log monitoring** architecture bullet; replaced the endpoints/auth paragraph (now lists `/metrics`, `/disk-eaters`, `/info`, `/critical-errors`); new **Recent critical errors** section after **Metrics** with curl examples.

Also updated (not counted above): `AGENTS.md` — serve bullet now says four endpoints and describes `/critical-errors`; the stale "There are no test files" claims were corrected since this change adds the first test file.

## Key changes

- Buffer is package-level in `internal/monitor`, mirroring the existing global Prometheus metric pattern: one `sync.Mutex`, writers are the rare per-shop tail goroutines, readers copy under lock and render outside it. Capacity 100/shop hardcoded (brainstorm decision — no config knob).
- Entries store the verbatim log line + capture timestamp; snapshots are returned newest-first. JSON payload carries `agent_started_at` to make restart-reset explicit.
- Midnight rotation needs no extra code: the package-level store outlives the recreated consumer goroutine.
- Dependency direction stays strictly `cmd → monitor`: `startTime` is passed into `CriticalErrorsHandler` rather than read globally.

## Deviations from plan

1. **Two bugs fixed in the plan's test file** (implementation semantics were authoritative):
   - `TestRecordCriticalEvictsOldest` asserted index `[0]` = oldest, but `snapshotCritical` documents/returns newest-first; assertions inverted accordingly.
   - `empty state` subtest omitted `format=text`, so it correctly received JSON (the plan specifies empty-state text for *text formats only*); `&format=text` added.
   - `limit clamp` subtest recorded 30 entries but expected 100 back; now records `criticalBufferSize+20`.
   - Per the plan's own note, hand-rolled `contains`/`indexOf` helpers and the dead `if want := …` placeholder were replaced with `strings.Contains` / deleted.
2. **`gofmt -w cmd/root.go cmd/serve.go`**: both files had pre-existing whitespace-only formatting violations; the Phase-4 gate requires clean `gofmt -l .`. Pure alignment changes.
3. **AGENTS.md "no tests" corrections**: beyond the planned endpoints-bullet edit, the Commands section claimed "no tests in repo" — factually wrong after Phase 1, so it was updated (also documents that the daily log file must exist before `serve` starts).
4. **Smoke-test procedure adjustment**: the plan's sequence appends log lines *after* starting `serve`, but when the daily log file doesn't exist yet at attach time, the tail's documented seek-to-EOF behaviour skips everything written up to/before creation. Re-ran with the file pre-created (as AGENTS.md already advises); first run actually validated the skip-by-design behaviour too.

## Technical decisions

- Kept all brainstorm decisions verbatim (recent history vs latest-only; in-memory buffer fed by tailer; fixed 100 lines/shop; keep rolling across midnight; endpoint param set). No new hard-to-reverse architectural decision was introduced beyond what brainstorm `latest-critical-errors-endpoint` already recorded, so no additional ADR was written.
- Test isolation via `resetCriticalStore()`; tests never start `serve`, so the `promauto` global registry stays safe (documented panic risk avoided).

## Testing notes

Static gates — all clean:

```
gofmt -l .          → (empty)
go build ./...      → ok
go vet ./...        → ok
go test ./...       → ok  github.com/topdata/node-agent/internal/monitor
go test ./internal/monitor/ -run 'Critical' -v → 5/5 PASS (incl. 6 handler subtests)
```

Manual smoke test (binary built, `:19144`, synthetic shop):

- Appended `.CRITICAL:` line + `[critical]` line + one `app.WARNING:` line → exactly the two critical lines captured, warning excluded, full untruncated messages, newest first (`total: 2`).
- `?shop=demo-shop` filtered correctly; `?shop=nope` → HTTP 200 with empty `shops{}` (never 404).
- `?limit=1&format=text` → one row, text block format correct; `Accept: text/markdown` negotiated to markdown table with pipe escaping.
- Typo path `/critical-events` → 404; wrong password → 401; no auth → 401.
- Restart spot-check: killed `serve`, restarted → empty result with fresh `agent_started_at`.

## Usage examples

```sh
curl -su USER:PASSWORD 'http://host:9144/critical-errors'                    # all shops, JSON
curl -su USER:PASSWORD 'http://host:9144/critical-errors?shop=muster-shop'   # one shop
curl -su USER:PASSWORD 'http://host:9144/critical-errors?limit=5&format=markdown'
curl -su USER:PASSWORD -H 'Accept: text/plain' 'http://host:9144/critical-errors'
```

## Documentation updates

- `CHANGELOG.md`: merged single `[Unreleased]` with Added entries (endpoint + first unit tests).
- `README.md`: architecture bullet, auth/endpoints paragraph, new "Recent critical errors" section.
- `AGENTS.md`: serve routes bullet (four endpoints), test-file guidance, smoke-test caveat about pre-existing log lines being skipped by design.

## Next steps

- Consider persisting the buffer across restarts (à la `disk.state_file`) if cold-start blindness becomes an operational problem.
- If the buffer ever grows beyond trivial scope (per-shop limits, persistence, eviction policies), record an ADR.
- Planned follow-up from earlier ADR: migrate `hpcloud/tail` → `github.com/nxadm/tail` to drop the fsnotify `replace` directive (unrelated to this feature).
