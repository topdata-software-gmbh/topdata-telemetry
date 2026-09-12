---
alternatives: []
billingGroupId: ''
decisions:
- decided_at: '2026-08-25T11:36:11+00:00'
  decision: 'Payload = recent history: in-memory ring buffer of the last N critical
    lines per shop (timestamp + raw line), filterable by ?shop=.'
  factors: []
  question: ''
- decided_at: '2026-08-25T11:37:16+00:00'
  decision: Data source = in-memory ring buffer filled by the existing TailLog goroutine
    (no extra I/O); history is lost on agent restart by design.
  factors: []
  question: ''
- decided_at: '2026-08-25T11:39:19+00:00'
  decision: Ring buffer capacity = fixed 100 lines per shop, hardcoded (no new config
    key).
  factors: []
  question: ''
- decided_at: '2026-08-25T11:41:35+00:00'
  decision: 'Midnight rotation: buffer keeps rolling across day boundaries (entries
    carry timestamps); no per-day clearing.'
  factors: []
  question: ''
- decided_at: '2026-08-25T11:43:29+00:00'
  decision: New Basic-Auth endpoint /critical-errors with ?shop=<name>, ?limit=N (default
    20, max 100), ?format=json|text|markdown using the same Accept-header fallback
    as /disk-eaters and /info.
  factors: []
  question: ''
- decided_at: '2026-08-25T11:45:38+00:00'
  decision: Store the full raw log line untruncated — primary consumer is an AI agent
    that needs complete error messages for debugging/fixing. Memory bound stays acceptable
    (~10 shops x 100 lines x typical line length, a few MB worst case).
  factors: []
  question: ''
- decided_at: '2026-08-25T14:32:03+00:00'
  decision: Design finalized; detailed 6-phase implementation plan written to _ai/backlog/active/260825_1626__IMPLEMENTATION_PLAN__critical-errors-endpoint.md
    (buffer + tests, tail wiring, endpoint registration, verification, docs housekeeping,
    report).
  factors: []
  question: ''
documentType: BRAINSTORM
id: 50ee67e6-b3ce-4cb9-8d03-3b3288947934
kind: brainstorm
open_questions: []
projectId: topdata-node-agent-v2
protocol_version: '1'
status: decided
tags:
- brainstorm
title: latest-critical-errors endpoint
topic: latest-critical-errors endpoint
updatedAt: '2026-08-25T14:32:03+00:00'
workspaceId: ''
content_hash: 7b08b22695507970c2f0d8ca939f9b0e
---

# latest-critical-errors endpoint — Brainstorm

## Protocol Checklist

- [ ] Review the current project state first (files, docs, recent commits)
- [ ] Ask questions one at a time; prefer multiple choice when possible
- [ ] Focus on understanding: purpose, constraints, success criteria
- [ ] Propose 2-3 approaches with trade-offs; lead with the recommended option
- [ ] Present the design in 200-300 word sections, validating after each
- [ ] Apply YAGNI ruthlessly
- [ ] Record decisions with `ctx brainstorm decide`

## Open Questions


## Decisions


### D-1: Payload = recent history: in-memory ring buffer of the last N critical lines per shop (timestamp + raw line), filterable by ?shop=.
- Decided: 2026-08-25T11:36:11+00:00

### D-2: Data source = in-memory ring buffer filled by the existing TailLog goroutine (no extra I/O); history is lost on agent restart by design.
- Decided: 2026-08-25T11:37:16+00:00

### D-3: Ring buffer capacity = fixed 100 lines per shop, hardcoded (no new config key).
- Decided: 2026-08-25T11:39:19+00:00

### D-4: Midnight rotation: buffer keeps rolling across day boundaries (entries carry timestamps); no per-day clearing.
- Decided: 2026-08-25T11:41:35+00:00

### D-5: New Basic-Auth endpoint /critical-errors with ?shop=<name>, ?limit=N (default 20, max 100), ?format=json|text|markdown using the same Accept-header fallback as /disk-eaters and /info.
- Decided: 2026-08-25T11:43:29+00:00

### D-6: Store the full raw log line untruncated — primary consumer is an AI agent that needs complete error messages for debugging/fixing. Memory bound stays acceptable (~10 shops x 100 lines x typical line length, a few MB worst case).
- Decided: 2026-08-25T11:45:38+00:00

### D-7: Design finalized; detailed 6-phase implementation plan written to _ai/backlog/active/260825_1626__IMPLEMENTATION_PLAN__critical-errors-endpoint.md (buffer + tests, tail wiring, endpoint registration, verification, docs housekeeping, report).
- Decided: 2026-08-25T14:32:03+00:00

## Alternatives