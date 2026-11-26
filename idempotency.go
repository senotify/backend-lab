package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Lab 02: the same "charge the customer" endpoint written four ways, hit by a
// simulated client that double-clicks, loses responses, retries in a storm,
// or reuses a key by mistake. Every run gets its own run_id so visitors never
// see each other's data, and the money that "moved" is whatever landed in
// idem_charges: the payment provider's ledger.

type Design struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Blurb string `json:"blurb"`
	Code  string `json:"code"`
}

var Designs = []Design{
	{"naive", "No idempotency", "Every request charges. Retries and double clicks charge again.",
		"charge(amount)\nreturn 201"},
	{"check_then_insert", "Check, then insert", "Looks the key up first and only charges if it's missing. Two requests that arrive together both see 'missing' and both charge.",
		"row := SELECT ... FROM idem_keys WHERE key = $1\nif row exists { return row.response }\ncharge(amount)          -- slow network call\nINSERT INTO idem_keys (key, response) ...\nreturn 201"},
	{"unique_only", "Unique constraint only", "Claims the key with INSERT ... ON CONFLICT DO NOTHING before charging. Never double charges, but a duplicate just gets 409, the original result is never replayed, and a failed charge leaves the key stuck forever.",
		"inserted := INSERT INTO idem_keys (key, status)\n            VALUES ($1, 'processing')\n            ON CONFLICT DO NOTHING RETURNING 1\nif !inserted { return 409 }\ncharge(amount)\nUPDATE idem_keys SET status = 'done' ..."},
	{"full", "Full idempotency key", "Claims the key atomically, stores a hash of the request and the final response. Replays the same response for retries, rejects a reused key with a different body (422), answers 409 while the first attempt is in flight, and releases the key if the charge fails so a retry can succeed.",
		"inserted := INSERT INTO idem_keys (key, request_hash, status)\n            VALUES ($1, $2, 'processing')\n            ON CONFLICT DO NOTHING RETURNING 1\nif !inserted {\n  row := SELECT ... FROM idem_keys WHERE key = $1\n  if row.request_hash != $2   { return 422 }   -- key reused for a different request\n  if row.status = 'processing' { return 409 }   -- first attempt still running\n  return row.response                          -- replay, same charge id\n}\nresult, err := charge(amount)\nif err != nil { DELETE FROM idem_keys WHERE key = $1; return 503 }  -- safe to retry\nUPDATE idem_keys SET status = 'done', response = result ..."},
}

type Scenario struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Blurb    string `json:"blurb"`
	Expected string `json:"expected"`
}

var Scenarios = []Scenario{
	{"double_click", "Double click", "The customer clicks Pay twice. Two identical requests arrive at the same moment.", "Charged exactly once."},
	{"lost_response", "Lost response", "The charge succeeds but the response is lost on the way back (mobile network). The client times out and retries, twice.", "Charged exactly once, and the retry gets the original receipt."},
	{"storm", "Retry storm", "A misbehaving client fires 10 identical requests at once.", "Charged exactly once."},
	{"key_reuse", "Key reused by mistake", "A client bug sends a new $99.00 order with the same idempotency key as an earlier $25.00 order.", "Charge $25.00 once and reject the $99.00 request, so the bug is visible."},
	{"provider_error", "Provider error, then retry", "The payment provider fails the first attempt (nothing charged). The client retries.", "Charged exactly once, on the retry."},
}

type AttemptResult struct {
	Attempt  int     `json:"attempt"`
	Amount   int     `json:"amount"`
	StartMs  float64 `json:"startMs"`
	EndMs    float64 `json:"endMs"`
	Status   int     `json:"status"`
	Note     string  `json:"note"`
	ChargeID int64   `json:"chargeId,omitempty"`
	Lost     bool    `json:"lost"`     // client gave up before the response arrived
	Replayed bool    `json:"replayed"` // server returned a stored response
}

type DesignRun struct {
	Design   string          `json:"design"`
	Attempts []AttemptResult `json:"attempts"`
	Charges  []ChargeRow     `json:"charges"`
	Pass     bool            `json:"pass"`
	Verdict  string          `json:"verdict"`
}

type ChargeRow struct {
	ID     int64 `json:"id"`
	Amount int   `json:"amount"`
}

type ScenarioRun struct {
	Scenario string      `json:"scenario"`
	Results  []DesignRun `json:"results"`
}

// ---------- the "payment provider" ----------

var errProvider = errors.New("provider unavailable")

// charge is the side effect we're protecting: a slow external call that moves
// money. failFirst makes the provider reject the first call of a run.
func (d *DB) charge(ctx context.Context, runID, design string, amount int, fail bool) (int64, error) {
	time.Sleep(time.Duration(120+mrand.IntN(120)) * time.Millisecond)
	if fail {
		return 0, errProvider
	}
	var id int64
	err := d.rw.QueryRow(ctx, `INSERT INTO idem_charges (run_id, design, amount_cents) VALUES ($1, $2, $3) RETURNING id`,
		runID, design, amount).Scan(&id)
	return id, err
}

// ---------- the four endpoint implementations ----------

type response struct {
	Status   int    `json:"status"`
	ChargeID int64  `json:"chargeId,omitempty"`
	Amount   int    `json:"amount,omitempty"`
	Error    string `json:"error,omitempty"`
	Replayed bool   `json:"-"`
}

type request struct {
	runID, design, key string
	amount             int
	providerFails      bool
}

func requestHash(r request) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("amount=%d", r.amount)))
	return hex.EncodeToString(h[:8])
}

func (d *DB) handle(ctx context.Context, r request) response {
	switch r.design {
	case "naive":
		id, err := d.charge(ctx, r.runID, r.design, r.amount, r.providerFails)
		if err != nil {
			return response{Status: 503, Error: "provider error"}
		}
		return response{Status: 201, ChargeID: id, Amount: r.amount}

	case "check_then_insert":
		var body []byte
		err := d.rw.QueryRow(ctx, `SELECT response FROM idem_keys WHERE run_id=$1 AND design=$2 AND key=$3`,
			r.runID, r.design, r.key).Scan(&body)
		if err == nil && body != nil {
			var resp response
			_ = json.Unmarshal(body, &resp)
			resp.Replayed = true
			return resp
		}
		id, err := d.charge(ctx, r.runID, r.design, r.amount, r.providerFails)
		if err != nil {
			return response{Status: 503, Error: "provider error"}
		}
		resp := response{Status: 201, ChargeID: id, Amount: r.amount}
		b, _ := json.Marshal(resp)
		// The race already happened above; this insert can't undo a second charge.
		_, _ = d.rw.Exec(ctx, `INSERT INTO idem_keys (run_id, design, key, status, response)
			VALUES ($1,$2,$3,'done',$4) ON CONFLICT DO NOTHING`, r.runID, r.design, r.key, b)
		return resp

	case "unique_only":
		tag, err := d.rw.Exec(ctx, `INSERT INTO idem_keys (run_id, design, key, status)
			VALUES ($1,$2,$3,'processing') ON CONFLICT DO NOTHING`, r.runID, r.design, r.key)
		if err != nil {
			return response{Status: 500, Error: err.Error()}
		}
		if tag.RowsAffected() == 0 {
			return response{Status: 409, Error: "duplicate request"}
		}
		id, err := d.charge(ctx, r.runID, r.design, r.amount, r.providerFails)
		if err != nil {
			// Bug on purpose: the key stays 'processing' forever.
			return response{Status: 503, Error: "provider error"}
		}
		_, _ = d.rw.Exec(ctx, `UPDATE idem_keys SET status='done' WHERE run_id=$1 AND design=$2 AND key=$3`, r.runID, r.design, r.key)
		return response{Status: 201, ChargeID: id, Amount: r.amount}

	case "full":
		hash := requestHash(r)
		tag, err := d.rw.Exec(ctx, `INSERT INTO idem_keys (run_id, design, key, request_hash, status)
			VALUES ($1,$2,$3,$4,'processing') ON CONFLICT DO NOTHING`, r.runID, r.design, r.key, hash)
		if err != nil {
			return response{Status: 500, Error: err.Error()}
		}
		if tag.RowsAffected() == 0 {
			var storedHash, status string
			var body []byte
			err := d.rw.QueryRow(ctx, `SELECT request_hash, status, response FROM idem_keys
				WHERE run_id=$1 AND design=$2 AND key=$3`, r.runID, r.design, r.key).Scan(&storedHash, &status, &body)
			if errors.Is(err, pgx.ErrNoRows) {
				// The first attempt failed and released the key a moment ago.
				return response{Status: 409, Error: "retry"}
			}
			if err != nil {
				return response{Status: 500, Error: err.Error()}
			}
			if storedHash != hash {
				return response{Status: 422, Error: "key reused with a different request"}
			}
			if status != "done" {
				return response{Status: 409, Error: "original request still in progress"}
			}
			var resp response
			_ = json.Unmarshal(body, &resp)
			resp.Replayed = true
			return resp
		}
		id, err := d.charge(ctx, r.runID, r.design, r.amount, r.providerFails)
		if err != nil {
			// Nothing was charged, so release the key: a retry may proceed.
			_, _ = d.rw.Exec(ctx, `DELETE FROM idem_keys WHERE run_id=$1 AND design=$2 AND key=$3`, r.runID, r.design, r.key)
			return response{Status: 503, Error: "provider error"}
		}
		resp := response{Status: 201, ChargeID: id, Amount: r.amount}
		b, _ := json.Marshal(resp)
		_, _ = d.rw.Exec(ctx, `UPDATE idem_keys SET status='done', response=$4
			WHERE run_id=$1 AND design=$2 AND key=$3`, r.runID, r.design, r.key, b)
		return resp
	}
	return response{Status: 400, Error: "unknown design"}
}

// ---------- the simulated client ----------

type clock struct{ t0 time.Time }

func (c clock) ms() float64 { return round2(float64(time.Since(c.t0).Microseconds()) / 1000) }

// call sends one request. If clientTimeout > 0 the client gives up after it,
// but the server keeps working (like a real dropped connection).
func (d *DB) call(r request, attempt int, clientTimeout time.Duration, c clock) AttemptResult {
	res := AttemptResult{Attempt: attempt, Amount: r.amount, StartMs: c.ms()}
	done := make(chan response, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- d.handle(ctx, r)
	}()
	var resp response
	if clientTimeout > 0 {
		select {
		case resp = <-done:
		case <-time.After(clientTimeout):
			res.EndMs = c.ms()
			res.Lost = true
			res.Note = "response lost, client timed out"
			// Let the server finish so its effects land before we count charges.
			<-done
			return res
		}
	} else {
		resp = <-done
	}
	res.EndMs = c.ms()
	res.Status = resp.Status
	res.ChargeID = resp.ChargeID
	res.Replayed = resp.Replayed
	switch {
	case resp.Replayed:
		res.Note = fmt.Sprintf("replayed receipt for charge #%d ($%s)", resp.ChargeID, dollars(resp.Amount))
	case resp.Status == 201:
		res.Note = fmt.Sprintf("charged $%s (charge #%d)", dollars(resp.Amount), resp.ChargeID)
	default:
		res.Note = resp.Error
	}
	return res
}

func (d *DB) runScenario(ctx context.Context, scenario, design string) (DesignRun, error) {
	runID := newRunID()
	c := clock{time.Now()}
	key := "order-" + runID[:6]
	base := request{runID: runID, design: design, key: key, amount: 2500}
	var attempts []AttemptResult

	concurrent := func(n int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		start := make(chan struct{})
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start // release them together, like real simultaneous clicks
				a := d.call(base, i, 0, c)
				mu.Lock()
				attempts = append(attempts, a)
				mu.Unlock()
			}(i)
		}
		close(start)
		wg.Wait()
	}

	switch scenario {
	case "double_click":
		concurrent(2)
	case "storm":
		concurrent(10)
	case "lost_response":
		attempts = append(attempts, d.call(base, 1, 60*time.Millisecond, c))
		time.Sleep(50 * time.Millisecond)
		attempts = append(attempts, d.call(base, 2, 60*time.Millisecond, c))
		time.Sleep(50 * time.Millisecond)
		attempts = append(attempts, d.call(base, 3, 0, c))
	case "key_reuse":
		attempts = append(attempts, d.call(base, 1, 0, c))
		second := base
		second.amount = 9900
		attempts = append(attempts, d.call(second, 2, 0, c))
	case "provider_error":
		first := base
		first.providerFails = true
		attempts = append(attempts, d.call(first, 1, 0, c))
		time.Sleep(50 * time.Millisecond)
		attempts = append(attempts, d.call(base, 2, 0, c))
	default:
		return DesignRun{}, fmt.Errorf("unknown scenario")
	}
	sortAttempts(attempts)

	rows, err := d.rw.Query(ctx, `SELECT id, amount_cents FROM idem_charges WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		return DesignRun{}, err
	}
	charges, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChargeRow, error) {
		var c ChargeRow
		err := r.Scan(&c.ID, &c.Amount)
		return c, err
	})
	if err != nil {
		return DesignRun{}, err
	}
	pass, verdict := judge(scenario, attempts, charges)
	return DesignRun{Design: design, Attempts: attempts, Charges: charges, Pass: pass, Verdict: verdict}, nil
}

// judge compares what actually happened with what should have.
func judge(scenario string, attempts []AttemptResult, charges []ChargeRow) (bool, string) {
	total := 0
	for _, c := range charges {
		total += c.Amount
	}
	n := len(charges)
	switch scenario {
	case "key_reuse":
		if n == 1 && charges[0].Amount == 2500 {
			if attempts[1].Status == 422 {
				return true, "Charged $25.00 once and rejected the reused key with 422."
			}
			if attempts[1].Status == 201 || attempts[1].Replayed {
				return false, "Charged $25.00, but told the client its $99.00 order succeeded. The bug stays hidden."
			}
			return false, fmt.Sprintf("Charged $25.00 once, but answered the $99.00 order with an unhelpful %d.", attempts[1].Status)
		}
		return false, fmt.Sprintf("Charged %d times, $%s in total. Both orders went through on one key.", n, dollars(total))
	case "provider_error":
		if n == 0 {
			return false, "The customer can never pay: the failed attempt left the key stuck as 'processing'."
		}
		if n == 1 {
			return true, "Charged exactly once, on the retry."
		}
	default:
		if n == 1 {
			last := attempts[len(attempts)-1]
			if scenario == "lost_response" && !last.Replayed && last.Status != 201 {
				return false, fmt.Sprintf("Charged once, but the retry got %d instead of the receipt. The client can't tell if it paid.", last.Status)
			}
			return true, "Charged exactly once."
		}
		if n == 0 {
			return false, "Nothing was charged."
		}
	}
	return false, fmt.Sprintf("Charged %d times: $%s for a $25.00 order.", n, dollars(total))
}

func sortAttempts(a []AttemptResult) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].StartMs < a[j-1].StartMs; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func dollars(cents int) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

func newRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RunScenarioAll runs one scenario against every design, one design at a time
// so they don't compete for the tiny connection pool.
func (d *DB) RunScenarioAll(ctx context.Context, scenario string) (*ScenarioRun, error) {
	out := &ScenarioRun{Scenario: scenario}
	for _, ds := range Designs {
		r, err := d.runScenario(ctx, scenario, ds.Key)
		if err != nil {
			return nil, err
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// cleanupIdem deletes run data older than an hour.
func (d *DB) cleanupIdem(ctx context.Context) {
	for _, q := range []string{
		`DELETE FROM idem_charges WHERE created_at < now() - interval '1 hour'`,
		`DELETE FROM idem_keys WHERE created_at < now() - interval '1 hour'`,
	} {
		if _, err := d.admin.Exec(ctx, q); err != nil {
			var pg *pgconn.PgError
			if !errors.As(err, &pg) {
				return
			}
		}
	}
}
