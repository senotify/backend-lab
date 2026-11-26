# Backend Lab

Interactive backend experiments measured live on real infrastructure:
**https://lab.senotify.com**

## Lab 01: OFFSET vs keyset pagination

A static 1,000,000-row Postgres table (20 rows/page, 50,000 pages, index on
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

## Running it

```sh
docker compose up --build     # http://localhost:8090, seeds 1M rows on first boot (~10s)
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
