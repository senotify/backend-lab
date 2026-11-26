package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every strategy returns the same 20 rows for a page; they differ only in how
// much work Postgres does to find them. The query text shown to visitors is
// exactly what runs.

type Strategy struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Blurb string `json:"blurb"`
}

var Strategies = []Strategy{
	{"offset", "Plain OFFSET", "Walks the index from the top and fetches every skipped row from the table, then throws them away."},
	{"deferred", "Deferred join", "Still OFFSET, but skips over the narrow index only (no table reads), then fetches just the 20 rows it returns."},
	{"keyset", "Keyset", "Seeks straight to the cursor (the last row of the previous page). Needs that cursor, so it fits Next/Prev, not typed page numbers."},
	{"checkpoint", "Checkpoint + small OFFSET", "Looks up a stored checkpoint every 1,000 rows, seeks there, then OFFSETs at most 999 rows. Any page, near-constant cost."},
	{"nearest", "Nearest end", "Plain OFFSET, but counts from whichever end is closer by reading the index backwards. The last page becomes cheap; the middle stays expensive."},
}

func strategyByKey(k string) (Strategy, bool) {
	for _, s := range Strategies {
		if s.Key == k {
			return s, true
		}
	}
	return Strategy{}, false
}

type Row struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Title     string    `json:"title"`
}

type RunResult struct {
	Strategy    string  `json:"strategy"`
	Page        int     `json:"page"`
	SQL         string  `json:"sql"`
	ExecutionMs float64 `json:"executionMs"`
	PlanningMs  float64 `json:"planningMs"`
	RowsTouched int64   `json:"rowsTouched"`
	Buffers     int64   `json:"buffers"` // 8KB pages read or found in cache
	Plan        string  `json:"plan"`
	TimedOut    bool    `json:"timedOut"`
	Cached      bool    `json:"cached"`
}

const orderDesc = "ORDER BY created_at DESC, id DESC"

func tsLit(t time.Time) string {
	return "'" + t.UTC().Format("2006-01-02 15:04:05.999999") + "+00'"
}

// cursorBefore returns the (created_at, id) of the row just before page p,
// i.e. what a keyset client would hold after viewing page p-1. Found via the
// checkpoints so it's cheap; it is setup, not part of what's measured.
func cursorBefore(ctx context.Context, pool *pgxpool.Pool, page int) (time.Time, int64, error) {
	idx := (page-1)*PageSize - 1 // 0-based index of the previous page's last row
	bucket, rem := idx/CheckpointStep, idx%CheckpointStep
	var ts time.Time
	var id int64
	err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT created_at, id FROM items
		WHERE (created_at, id) <= (SELECT created_at, id FROM item_checkpoints WHERE bucket = %d)
		ORDER BY created_at DESC, id DESC OFFSET %d LIMIT 1`, bucket, rem)).Scan(&ts, &id)
	return ts, id, err
}

func buildSQL(ctx context.Context, pool *pgxpool.Pool, strategy string, page int) (string, error) {
	off := (page - 1) * PageSize
	switch strategy {
	case "offset":
		return fmt.Sprintf("SELECT * FROM items\n%s\nOFFSET %d LIMIT %d", orderDesc, off, PageSize), nil
	case "deferred":
		return fmt.Sprintf("SELECT i.* FROM items i\nJOIN (\n  SELECT id FROM items\n  %s\n  OFFSET %d LIMIT %d\n) p USING (id)\nORDER BY i.created_at DESC, i.id DESC", orderDesc, off, PageSize), nil
	case "keyset":
		if page == 1 {
			return fmt.Sprintf("SELECT * FROM items\n%s\nLIMIT %d", orderDesc, PageSize), nil
		}
		ts, id, err := cursorBefore(ctx, pool, page)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SELECT * FROM items\nWHERE (created_at, id) < (%s, %d)\n%s\nLIMIT %d", tsLit(ts), id, orderDesc, PageSize), nil
	case "checkpoint":
		// The row subquery runs once (an InitPlan), so Postgres can use its
		// result as an index seek. A plain join against item_checkpoints can't,
		// and quietly walks the index from the top like OFFSET does.
		return fmt.Sprintf("SELECT * FROM items\nWHERE (created_at, id) <= (\n  SELECT created_at, id FROM item_checkpoints WHERE bucket = %d\n)\n%s\nOFFSET %d LIMIT %d", off/CheckpointStep, orderDesc, off%CheckpointStep, PageSize), nil
	case "nearest":
		if off <= TotalRows/2 {
			return fmt.Sprintf("SELECT * FROM items\n%s\nOFFSET %d LIMIT %d", orderDesc, off, PageSize), nil
		}
		fromEnd := TotalRows - off - PageSize
		return fmt.Sprintf("SELECT * FROM (\n  SELECT * FROM items\n  ORDER BY created_at ASC, id ASC  -- read the index backwards\n  OFFSET %d LIMIT %d\n) t\n%s", fromEnd, PageSize, orderDesc), nil
	}
	return "", fmt.Errorf("unknown strategy")
}

type planNode struct {
	NodeType     string     `json:"Node Type"`
	Relation     string     `json:"Relation Name"`
	Index        string     `json:"Index Name"`
	Direction    string     `json:"Scan Direction"`
	ActualRows   float64    `json:"Actual Rows"`
	ActualLoops  float64    `json:"Actual Loops"`
	HeapFetches  *float64   `json:"Heap Fetches"`
	SharedHit    int64      `json:"Shared Hit Blocks"`
	SharedRead   int64      `json:"Shared Read Blocks"`
	IndexCond    string     `json:"Index Cond"`
	Plans        []planNode `json:"Plans"`
}

type explainOut struct {
	Plan      planNode `json:"Plan"`
	Planning  float64  `json:"Planning Time"`
	Execution float64  `json:"Execution Time"`
}

func renderPlan(n planNode, depth int, b *strings.Builder) {
	pad := strings.Repeat("  ", depth)
	if depth > 0 {
		pad += "-> "
	}
	line := n.NodeType
	if n.Index != "" {
		line += " using " + n.Index
	}
	if n.Relation != "" && n.Index == "" {
		line += " on " + n.Relation
	}
	if n.Direction == "Backward" {
		line += " (backward)"
	}
	fmt.Fprintf(b, "%s%s  (rows=%s loops=%d)\n", pad, line, commas(int64(n.ActualRows)), int64(n.ActualLoops))
	if n.IndexCond != "" {
		fmt.Fprintf(b, "%s     Index Cond: %s\n", strings.Repeat("  ", depth), n.IndexCond)
	}
	if n.HeapFetches != nil {
		fmt.Fprintf(b, "%s     Heap Fetches: %s\n", strings.Repeat("  ", depth), commas(int64(*n.HeapFetches)))
	}
	for _, c := range n.Plans {
		renderPlan(c, depth+1, b)
	}
}

// rowsTouched counts rows produced by scans of the items table: the work
// that grows with page depth.
func rowsTouched(n planNode) int64 {
	var t int64
	if n.Relation == "items" && strings.Contains(n.NodeType, "Scan") {
		t += int64(n.ActualRows * n.ActualLoops)
	}
	for _, c := range n.Plans {
		t += rowsTouched(c)
	}
	return t
}

func explain(ctx context.Context, pool *pgxpool.Pool, sql string) (*explainOut, error) {
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql).Scan(&raw); err != nil {
		return nil, err
	}
	var out []explainOut
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil, fmt.Errorf("bad explain output: %v", err)
	}
	return &out[0], nil
}

func isTimeout(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "57014" || errors.Is(err, context.DeadlineExceeded)
}

func (d *DB) Run(ctx context.Context, strategy string, page int) (*RunResult, error) {
	sql, err := buildSQL(ctx, d.ro, strategy, page)
	if err != nil {
		return nil, err
	}
	res := &RunResult{Strategy: strategy, Page: page, SQL: sql}
	out, err := explain(ctx, d.ro, sql)
	if err != nil {
		if isTimeout(err) {
			res.TimedOut = true
			return res, nil
		}
		return nil, err
	}
	var b strings.Builder
	renderPlan(out.Plan, 0, &b)
	res.ExecutionMs = round2(out.Execution)
	res.PlanningMs = round2(out.Planning)
	res.RowsTouched = rowsTouched(out.Plan)
	res.Buffers = out.Plan.SharedHit + out.Plan.SharedRead
	res.Plan = strings.TrimRight(b.String(), "\n")
	return res, nil
}

// Rows fetches a page for display, always via the cheap checkpoint path.
func (d *DB) Rows(ctx context.Context, page int) ([]Row, error) {
	sql, _ := buildSQL(ctx, d.ro, "checkpoint", page)
	rows, err := d.ro.Query(ctx, strings.Replace(sql, "SELECT *", "SELECT id, created_at, title", 1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Title); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- sweep: latency vs depth for every strategy, measured on this server ----

type SweepPoint struct {
	Offset int                `json:"offset"`
	Page   int                `json:"page"`
	Ms     map[string]float64 `json:"ms"`
}

type Sweep struct {
	MeasuredAt time.Time    `json:"measuredAt"`
	Points     []SweepPoint `json:"points"`
}

var sweepDepths = []int{0, 1_000, 10_000, 100_000, 250_000, 500_000, TotalRows - PageSize}

func (d *DB) RunSweep(ctx context.Context) (*Sweep, error) {
	sw := &Sweep{MeasuredAt: time.Now().UTC()}
	for _, off := range sweepDepths {
		page := off/PageSize + 1
		pt := SweepPoint{Offset: off, Page: page, Ms: map[string]float64{}}
		for _, s := range Strategies {
			sql, err := buildSQL(ctx, d.admin, s.Key, page)
			if err != nil {
				return nil, err
			}
			var times []float64
			for i := 0; i < 3; i++ {
				out, err := explain(ctx, d.admin, sql)
				if err != nil {
					return nil, err
				}
				times = append(times, out.Execution)
			}
			sort.Float64s(times)
			pt.Ms[s.Key] = round2(times[1])
		}
		sw.Points = append(sw.Points, pt)
	}
	return sw, nil
}

// ---- small helpers ----

type resultCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}
type cacheEntry struct {
	r   RunResult
	exp time.Time
}

func (c *resultCache) get(k string) (RunResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		return RunResult{}, false
	}
	return e.r, true
}

func (c *resultCache) put(k string, r RunResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 2000 {
		c.m = map[string]cacheEntry{}
	}
	c.m[k] = cacheEntry{r, time.Now().Add(10 * time.Minute)}
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
