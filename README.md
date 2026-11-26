# Backend Lab

Interactive backend experiments measured live on real infrastructure:
**https://lab.senotify.com**

## Lab 01: OFFSET vs keyset pagination

A static 10,000,000-row Postgres table (20 rows/page, 500,000 pages, index on
`(created_at DESC, id DESC)`). Visitors pick any page; five strategies fetch the
same 20 rows and the page shows `EXPLAIN (ANALYZE, BUFFERS)` timing, rows
touched and the plan for each:

| Strategy | Idea | Cost vs depth |
|---|---|---|
| Plain OFFSET | walk the index, fetch and discard skipped rows | linear |
| Deferred join | OFFSET over the narrow index only, then fetch 20 rows | linear, ~3x cheaper |
| Keyset | seek to the previous page's last row | constant |
| Checkpoint | stored key every 1,000 rows, seek there, OFFSET ≤ 999 | ~constant, any page |
| Nearest end | OFFSET from whichever end is closer (reads index backwards) | cheap at both ends |

Gotcha found while building it: joining the checkpoint table
(`FROM items, item_checkpoints c WHERE (i.created_at, i.id) <= (c.created_at, c.id)`)
does **not** seek; Postgres walks the index from the top like OFFSET. A row
subquery (`<= (SELECT created_at, id FROM item_checkpoints WHERE bucket = N)`)
becomes an InitPlan and is used as an `Index Cond`.

## Lab 02: Idempotency keys

One "charge $25.00" endpoint written four ways (no idempotency, check-then-insert,
unique constraint only, full idempotency key), each hit by a simulated client:
double click, lost response + retries, a 10-request storm, a key reused for a
different order, and a provider error followed by a retry. Charges land in an
`idem_charges` ledger, so the verdict is what actually happened to the card.

| | No idempotency | Check, then insert | Unique only | Full key |
|---|---|---|---|---|
| Double click | ✗ 2 charges | ✗ 2 charges (race) | ✓ | ✓ |
| Lost response | ✗ 3 charges | ✓ | ✗ retry gets 409, no receipt | ✓ replay |
| Retry storm | ✗ 10 charges | ✗ 10 charges | ✓ | ✓ |
| Key reused | ✗ both charged | ✗ $99 order "succeeds" silently | ✗ opaque 409 | ✓ 422 |
| Provider error | ✓ | ✓ | ✗ key stuck, can never pay | ✓ |

Visitor runs use a separate `lab_rw` role that can only touch the `idem_*`
tables; each run has its own `run_id` and data older than an hour is deleted.

## Running it

```sh
docker compose up --build     # http://localhost:8090, seeds 10M rows on first boot (a few minutes)
```

## Guardrails (it shares a 2 vCPU VPS with real apps)

- Visitor queries run as `lab_ro`: read-only transactions, `statement_timeout = 5s`
- 3-connection pool for visitor queries, so a burst of deep-OFFSET clicks queues
  instead of starving the box
- 30 runs/minute per IP, identical runs cached for 10 minutes
- Postgres capped at 640 MB, on a private `lab-net` network with no published port
- The Go service uses ~5 MB RAM (`--memory 128m`)

## Deploy

Push to `master` → GHCR → VPS (`.github/workflows/deploy.yml`). Postgres
(`lab-postgres`, volume `lab-pgdata`) is created once and left alone; the app
container is replaced each deploy. Secrets: `VPS_HOST`, `VPS_USER`,
`VPS_SSH_KEY`, `LAB_DB_PASSWORD`. Caddy routes `lab.senotify.com → lab:8080`.
