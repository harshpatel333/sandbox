//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type matchingFixture struct {
	t         *testing.T
	f         *entitlementFixture
	client    *stripeHTTPClient
	mu        sync.Mutex
	invoices  map[string]StripeInvoiceAmounts
	grants    map[string]entitlementGrant
	byKey     map[string]string
	posts     []url.Values
	keys      []string
	lose      int
	refunded  bool
	unpaid    map[string]bool
	periodEnd time.Time
}

func newMatchingFixture(t *testing.T, basisPoints int32, capCents, prior int64) *matchingFixture {
	t.Helper()
	m := &matchingFixture{t: t, f: newEntitlementFixture(t), invoices: map[string]StripeInvoiceAmounts{}, grants: map[string]entitlementGrant{}, byKey: map[string]string{}, unpaid: map[string]bool{}}
	m.periodEnd = m.f.start
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	m.client = &stripeHTTPClient{baseURL: srv.URL, apiVersion: "2026-07-29.dahlia", httpClient: srv.Client()}
	m.f.h.Stripe = m.client
	if err := insertContractMatchingRule(t.Context(), m.f.h.Pool, matchingRule{ContractID: m.f.c.ID, TeamID: m.f.c.TeamID, Terms: matchingTerms{BasisPoints: basisPoints, CapCents: capCents}, PriorMatchedCents: prior, EffectiveFrom: m.f.c.StartsAt, Source: "plan"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.retire)
	return m
}

// The sweep is fleet-wide, so a finished test parks its rows where later
// tests' sweeps cannot reach them.
func (m *matchingFixture) retire() {
	ctx := context.Background()
	conn, err := m.f.h.Pool.Acquire(ctx)
	if err != nil {
		m.t.Error(err)
		return
	}
	defer conn.Release()
	for _, sql := range []string{
		`SET session_replication_role=replica`,
		`UPDATE billing_contract_matching_rule SET effective_from='9999-01-01' WHERE contract_id=$1`,
		`UPDATE billing_matching_credit SET next_attempt_at='9999-01-01',review_checked_at='9999-01-01' WHERE contract_id=$1`,
		`SET session_replication_role=origin`,
	} {
		var args []any
		if strings.Contains(sql, "$1") {
			args = append(args, m.f.c.ID)
		}
		if _, err = conn.Exec(ctx, sql, args...); err != nil {
			m.t.Error(err)
		}
	}
}

func (m *matchingFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/invoices/in_"):
		inv, ok := m.invoices[strings.TrimPrefix(r.URL.Path, "/v1/invoices/")]
		if !ok {
			http.Error(w, "missing", 404)
			return
		}
		if m.unpaid[inv.ID] {
			inv.Status = "open"
			remaining := int64(100)
			inv.AmountRemaining = &remaining
		}
		json.NewEncoder(w).Encode(inv)
	case r.URL.Path == "/v1/billing/credit_grants" && r.Method == http.MethodPost:
		if err := r.ParseForm(); err != nil {
			m.t.Error(err)
		}
		key := r.Header.Get("Idempotency-Key")
		m.posts = append(m.posts, r.Form)
		m.keys = append(m.keys, key)
		id, ok := m.byKey[key]
		if !ok {
			id = "credgr_match_" + uuid.NewString()
			var g entitlementGrant
			g.ID, g.Customer, g.Category, g.Priority = id, r.Form.Get("customer"), r.Form.Get("category"), 50
			g.Created, g.EffectiveAt = time.Now().Unix(), time.Now().Unix()
			g.Amount.Type, g.Amount.Monetary.Currency = "monetary", "usd"
			fmt.Sscan(r.Form.Get("amount[monetary][value]"), &g.Amount.Monetary.Value)
			g.Applicability.Scope.PriceType = r.Form.Get("applicability_config[scope][price_type]")
			g.Metadata = map[string]string{"matching_credit_id": r.Form.Get("metadata[matching_credit_id]"), "invoice_id": r.Form.Get("metadata[invoice_id]")}
			m.grants[id] = g
			m.byKey[key] = id
		}
		if m.lose > 0 {
			m.lose--
			http.Error(w, `{"error":{"message":"response lost"}}`, 503)
			return
		}
		json.NewEncoder(w).Encode(m.grants[id])
	case r.URL.Path == "/v1/billing/credit_grants":
		data := []entitlementGrant{}
		for _, g := range m.grants {
			data = append(data, g)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	case strings.HasPrefix(r.URL.Path, "/v1/billing/credit_grants/"):
		json.NewEncoder(w).Encode(m.grants[strings.TrimPrefix(r.URL.Path, "/v1/billing/credit_grants/")])
	case r.URL.Path == "/v1/invoice_payments":
		inv := r.URL.Query().Get("invoice")
		fmt.Fprintf(w, `{"data":[{"invoice":%q,"amount_paid":100,"payment":{"type":"payment_intent","payment_intent":"pi_match"}}]}`, inv)
	case r.URL.Path == "/v1/payment_intents/pi_match":
		fmt.Fprint(w, `{"id":"pi_match","latest_charge":"ch_match"}`)
	case r.URL.Path == "/v1/charges/ch_match":
		refunded := 0
		if m.refunded {
			refunded = 100
		}
		fmt.Fprintf(w, `{"id":"ch_match","customer":%q,"payment_intent":"pi_match","currency":"usd","paid":true,"captured":true,"amount_captured":100,"amount_refunded":%d}`, m.f.c.CustomerID, refunded)
	default:
		m.t.Errorf("unexpected provider request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", 500)
	}
}

func usageLine(price string, amount int64, credits ...StripeInvoicePretaxCredit) StripeInvoiceAmountLine {
	var line StripeInvoiceAmountLine
	line.ID, line.Amount, line.Currency = "il_"+uuid.NewString(), amount, "usd"
	line.Pricing.PriceDetails = &struct {
		Price   string `json:"price"`
		Product string `json:"product"`
	}{Price: price}
	line.PretaxCredits = credits
	return line
}

func applied(amount int64) StripeInvoicePretaxCredit {
	return StripeInvoicePretaxCredit{Type: "credit_balance_transaction", Amount: amount, CreditBalanceTransaction: "cbtxn_" + uuid.NewString()}
}

// paidInvoice stages one released, collected usage invoice for the next
// monthly period and returns its ID.
func (m *matchingFixture) paidInvoice(collection string, lines ...StripeInvoiceAmountLine) string {
	m.t.Helper()
	start := m.periodEnd
	end := start.AddDate(0, 1, 0)
	m.periodEnd = end
	return m.invoiceFor(start, end, "released", collection, lines...)
}

func (m *matchingFixture) invoiceFor(start, end time.Time, closeState, collection string, lines ...StripeInvoiceAmountLine) string {
	m.t.Helper()
	id := "in_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	zero := int64(0)
	inv := StripeInvoiceAmounts{ID: id, Customer: m.f.c.CustomerID, Currency: "usd", Status: "paid", AmountRemaining: &zero, BillingReason: "subscription_cycle", PeriodStart: start.Unix(), PeriodEnd: end.Unix(), CollectionMethod: "charge_automatically"}
	inv.Parent.SubscriptionDetails = &struct {
		Subscription string `json:"subscription"`
	}{Subscription: m.f.c.SubscriptionID}
	inv.Lines.Data = lines
	m.mu.Lock()
	m.invoices[id] = inv
	m.mu.Unlock()
	plan := map[string]any{"customer": m.f.c.CustomerID, "subscription": m.f.c.SubscriptionID, "credit_version": 1, "credit_prices": []string{"price_cpu", "price_memory", "price_round", "price_storage"}, "base_cents": 100000, "expected_cents": 0, "credit_before_cents": 0, "resources": []any{}}
	raw, _ := json.Marshal(plan)
	m.exec(`INSERT INTO team_billing_period(team_id,period_start,period_end,status,finalized_at,gross_charges_usd,credits_applied_usd,net_invoice_amount_usd) VALUES($1,$2,$3,'finalized',clock_timestamp(),0,0,0)`, m.f.c.TeamID, start, end)
	m.exec(`INSERT INTO billing_invoice_close(team_id,period_start,period_end,invoice_id,state,verified_at,plan,finalized_evidence) VALUES($1,$2,$3,$4,$6,clock_timestamp(),$5,'{}')`, m.f.c.TeamID, start, end, id, raw, closeState)
	m.exec(`INSERT INTO billing_invoice_collection(invoice_id,customer_id,subscription_id,livemode,settlement_hash,idempotency_key,payment_authorized,state) VALUES($1,$2,$3,false,$4,$5,true,$6)`, id, m.f.c.CustomerID, m.f.c.SubscriptionID, invoiceSettlementHash(inv), "billing-invoice-pay:v1:"+id, collection)
	return id
}

func (m *matchingFixture) exec(sql string, args ...any) {
	m.t.Helper()
	if _, err := m.f.h.Pool.Exec(m.t.Context(), sql, args...); err != nil {
		m.t.Fatal(err)
	}
}

func (m *matchingFixture) tick() error {
	m.exec(`UPDATE billing_matching_credit SET next_attempt_at=clock_timestamp() WHERE contract_id=$1`, m.f.c.ID)
	return m.f.h.matchingCreditTick(m.t.Context(), m.client)
}

type matchingRow struct {
	State, Skip                    string
	Usage, Credit, Matchable, Paid int64
	Grant                          string
	Flag                           string
}

func (m *matchingFixture) row(invoice string) matchingRow {
	m.t.Helper()
	var r matchingRow
	err := m.f.h.Pool.QueryRow(m.t.Context(), `SELECT state,COALESCE(skip_reason,''),usage_cents,credit_applied_cents,matchable_cents,amount_cents,COALESCE(stripe_credit_grant_id,''),COALESCE(flag_reason,'') FROM billing_matching_credit WHERE invoice_id=$1`, invoice).
		Scan(&r.State, &r.Skip, &r.Usage, &r.Credit, &r.Matchable, &r.Paid, &r.Grant, &r.Flag)
	if err != nil {
		m.t.Fatalf("matching row for %s: %v", invoice, err)
	}
	return r
}

func TestMatchingCreditProWorkedExample(t *testing.T) {
	m := newMatchingFixture(t, 5000, 1_000_000, 0)
	invoice := m.paidInvoice("paid", usageLine("price_base", 100000), usageLine("price_cpu", 50000, applied(25000)))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	r := m.row(invoice)
	if r.State != "issued" || r.Usage != 50000 || r.Credit != 25000 || r.Matchable != 25000 || r.Paid != 12500 {
		t.Fatalf("fee or credited usage was matched: %+v", r)
	}
	if len(m.posts) != 1 || m.keys[0] != "matching-credit:v1:"+invoice {
		t.Fatalf("grant identity not derived from invoice: %v", m.keys)
	}
	form := m.posts[0]
	for k, want := range map[string]string{"amount[monetary][value]": "12500", "applicability_config[scope][price_type]": "metered", "priority": "50", "category": "promotional", "metadata[invoice_id]": invoice, "customer": m.f.c.CustomerID} {
		if form.Get(k) != want {
			t.Fatalf("wire %s = %q, want %q", k, form.Get(k), want)
		}
	}
	if form.Has("expires_at") || form.Has("effective_at") || form.Has("applicability_config[scope][prices][0][id]") {
		t.Fatal("matched credit must not expire and must cover all metered usage", form)
	}
	g := m.grants[r.Grant]
	if err := m.f.h.verifyOwnedEntitlementGrant(t.Context(), g); err != nil {
		t.Fatal("issued matching grant rejected by invoice credit ownership check", err)
	}
	if err := creditScope(g, m.f.c.CustomerID, time.Now().Add(400*24*time.Hour).Unix(), []string{"price_cpu"}); err != nil {
		t.Fatal("invoice close rejects matching grant scope", err)
	}
	g.Amount.Monetary.Value++
	if err := m.f.h.verifyOwnedEntitlementGrant(t.Context(), g); err == nil {
		t.Fatal("altered matching grant accepted")
	}
	g.ID = "credgr_unknown"
	if err := m.f.h.verifyOwnedEntitlementGrant(t.Context(), g); err == nil {
		t.Fatal("unconfirmed matching grant accepted")
	}
}

func TestMatchingCreditLegacyYCSchedule(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	first := m.paidInvoice("paid", usageLine("price_cpu", 500000, applied(500000)))
	second := m.paidInvoice("paid", usageLine("price_cpu", 400000, applied(400000)), usageLine("price_memory", 300000, applied(200000)))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(first); r.State != "skipped" || r.Paid != 0 || r.Skip != "" {
		t.Fatalf("fully credited month matched: %+v", r)
	}
	if r := m.row(second); r.State != "issued" || r.Matchable != 100000 || r.Paid != 100000 {
		t.Fatalf("customer-paid usage not matched: %+v", r)
	}
	consumed := m.paidInvoice("paid", usageLine("price_cpu", 100000, applied(100000)))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(consumed); r.State != "skipped" || r.Paid != 0 {
		t.Fatalf("usage covered by earlier matching credit compounded: %+v", r)
	}
	if len(m.posts) != 1 {
		t.Fatalf("expected one grant, got %d", len(m.posts))
	}
}

func TestMatchingCreditReplayAndDuplicates(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	invoice := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	m.lose = 1
	if err := m.tick(); err == nil {
		t.Fatal("expected lost provider response")
	}
	if r := m.row(invoice); r.State != "uncertain" {
		t.Fatalf("lost response not durable: %+v", r)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.f.h.matchingCreditTick(context.WithoutCancel(t.Context()), m.client)
		}()
	}
	wg.Wait()
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(invoice); r.State != "issued" || r.Paid != 70000 {
		t.Fatalf("replay did not settle: %+v", r)
	}
	if len(m.grants) != 1 {
		t.Fatalf("replay created %d grants", len(m.grants))
	}
	for i := range m.posts {
		if m.keys[i] != m.keys[0] || m.posts[i].Encode() != m.posts[0].Encode() {
			t.Fatal("replay changed the frozen request")
		}
	}
	var rows int
	if err := m.f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_matching_credit WHERE contract_id=$1`, m.f.c.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("invoice matched more than once", rows, err)
	}
	posts := len(m.posts)
	m.exec(`UPDATE billing_invoice_collection SET next_attempt_at=clock_timestamp() WHERE invoice_id=$1`, invoice)
	if err := m.tick(); err != nil || len(m.posts) != posts {
		t.Fatal("duplicate paid event re-issued", err)
	}
}

func TestMatchingCreditRequiresFullPayment(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	open := m.paidInvoice("processing", usageLine("price_cpu", 70000))
	partial := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	m.unpaid[partial] = true
	if err := m.tick(); err == nil {
		t.Fatal("partially paid invoice was accepted")
	}
	var rows int
	if err := m.f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM billing_matching_credit WHERE contract_id=$1`, m.f.c.ID).Scan(&rows); err != nil || rows != 0 || len(m.posts) != 0 {
		t.Fatal("matched before full payment", rows, err)
	}
	m.exec(`UPDATE billing_invoice_collection SET state='paid' WHERE invoice_id=$1`, open)
	m.mu.Lock()
	delete(m.unpaid, partial)
	m.mu.Unlock()
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if m.row(open).Paid != 70000 || m.row(partial).Paid != 70000 {
		t.Fatal("settled invoices were not matched")
	}
}

func TestMatchingCreditCapAcrossOutOfOrderInvoices(t *testing.T) {
	m := newMatchingFixture(t, 10000, 150000, 20000)
	first := m.paidInvoice("processing", usageLine("price_cpu", 100000))
	second := m.paidInvoice("processing", usageLine("price_cpu", 100000))
	third := m.paidInvoice("paid", usageLine("price_cpu", 100000))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	m.exec(`UPDATE billing_invoice_collection SET state='paid' WHERE invoice_id IN ($1,$2)`, first, second)
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	total := m.row(first).Paid + m.row(second).Paid + m.row(third).Paid
	if m.row(third).Paid != 100000 || total != 130000 {
		t.Fatalf("cap overshot or misallocated: %d", total)
	}
	if r := m.row(second); r.State != "skipped" || r.Paid != 0 {
		t.Fatalf("exhausted cap still matched: %+v", r)
	}
}

func TestMatchingCreditWaitsForInvoiceClose(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	invoice := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	closing := m.invoiceFor(m.periodEnd, m.periodEnd.AddDate(0, 1, 0), "prepared", "pending")
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(invoice); r.State != "pending" || len(m.posts) != 0 {
		t.Fatalf("grant issued during an invoice close: %+v", r)
	}
	m.exec(`UPDATE billing_invoice_close SET state='released' WHERE invoice_id=$1`, closing)
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(invoice); r.State != "issued" {
		t.Fatalf("grant not issued after close released: %+v", r)
	}
}

func TestMatchingCreditFlagsReversals(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	noted := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	refunded := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	inv := m.invoices[noted]
	inv.PostPaymentCreditNotes = 5000
	m.invoices[noted] = inv
	m.refunded = true
	m.mu.Unlock()
	m.exec(`UPDATE billing_matching_credit SET review_checked_at=NULL WHERE contract_id=$1`, m.f.c.ID)
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	for _, invoice := range []string{noted, refunded} {
		if r := m.row(invoice); r.Flag == "" || r.State != "issued" || r.Paid != 70000 {
			t.Fatalf("reversal not flagged or credit clawed back: %+v", r)
		}
	}
	changed := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	m.mu.Lock()
	inv = m.invoices[changed]
	inv.Total = 1
	m.invoices[changed] = inv
	m.mu.Unlock()
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(changed); r.State != "skipped" || r.Skip == "" || r.Flag == "" {
		t.Fatalf("changed settlement was matched: %+v", r)
	}
}

func TestMatchingCreditDiscoveryAfterReplayWindow(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	invoice := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	m.lose = 1
	_ = m.tick()
	conn, err := m.f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(t.Context(), `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), `UPDATE billing_matching_credit SET first_attempt_at=first_attempt_at-interval '1 day' WHERE invoice_id=$1`, invoice); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), `SET session_replication_role=origin`); err != nil {
		t.Fatal(err)
	}
	posts := len(m.posts)
	if err = m.tick(); err != nil {
		t.Fatal(err)
	}
	if r := m.row(invoice); r.State != "issued" || len(m.posts) != posts {
		t.Fatalf("expired replay window resent instead of discovering: %+v", r)
	}
}

func TestMatchingCreditLedgerIsImmutable(t *testing.T) {
	m := newMatchingFixture(t, 10000, 8_000_000, 0)
	invoice := m.paidInvoice("paid", usageLine("price_cpu", 70000))
	if err := m.tick(); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE billing_matching_credit SET amount_cents=amount_cents+1 WHERE invoice_id=$1`,
		`UPDATE billing_matching_credit SET state='pending' WHERE invoice_id=$1`,
		`DELETE FROM billing_matching_credit WHERE invoice_id=$1`,
	} {
		if _, err := m.f.h.Pool.Exec(t.Context(), sql, invoice); err == nil {
			t.Fatalf("ledger accepted %s", sql)
		}
	}
	for _, sql := range []string{
		`UPDATE billing_contract_matching_rule SET cap_cents=cap_cents+1 WHERE contract_id=$1`,
		`DELETE FROM billing_contract_matching_rule WHERE contract_id=$1`,
	} {
		if _, err := m.f.h.Pool.Exec(t.Context(), sql, m.f.c.ID); err == nil {
			t.Fatalf("rule accepted %s", sql)
		}
	}
	unpaid := m.invoiceFor(m.periodEnd, m.periodEnd.AddDate(0, 1, 0), "released", "processing", usageLine("price_cpu", 1))
	if _, err := m.f.h.Pool.Exec(t.Context(), `INSERT INTO billing_matching_credit(team_id,contract_id,invoice_id,customer_id,period_start,period_end,usage_cents,credit_applied_cents,discount_cents,matchable_cents,amount_cents,invoice_evidence,state,idempotency_key)
 SELECT team_id,$2,invoice_id,$3,period_start,period_end,1,0,0,0,0,'{}','skipped','' FROM billing_invoice_close WHERE invoice_id=$1`, unpaid, m.f.c.ID, m.f.c.CustomerID); err == nil {
		t.Fatal("ledger accepted an unpaid invoice")
	}
}
