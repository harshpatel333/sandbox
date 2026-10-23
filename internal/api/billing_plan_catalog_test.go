package api

import (
	"testing"
)

func int32p(v int32) *int32 { return &v }
func int64p(v int64) *int64 { return &v }

func TestExpandPlanScheduleTimedGrants(t *testing.T) {
	yc := []planGrant{
		{StartMonths: 0, Repeat: int32p(3), EveryMonths: 1, AmountCents: 3000000, ExpiresAfterMonths: 1},
		{StartMonths: 3, Repeat: int32p(9), EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1},
	}
	issues := expandPlanSchedule(yc, 24)
	if len(issues) != 12 {
		t.Fatalf("issues=%d", len(issues))
	}
	for i, issue := range issues {
		want := int64(100000)
		if i < 3 {
			want = 3000000
		}
		if issue.MonthOffset != int32(i) || issue.AmountCents != want || issue.ExpiresAfterMonths != 1 {
			t.Fatalf("issue %d = %+v", i, issue)
		}
	}
	legacy := []planGrant{
		{StartMonths: 0, Repeat: int32p(1), EveryMonths: 1, AmountCents: 1000000, ExpiresAfterMonths: 3},
		{StartMonths: 0, EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1},
	}
	issues = expandPlanSchedule(legacy, 4)
	if len(issues) != 5 || issues[0].AmountCents != 1000000 || issues[0].ExpiresAfterMonths != 3 || issues[4].MonthOffset != 3 {
		t.Fatalf("legacy issues=%+v", issues)
	}
	quarterly := expandPlanSchedule([]planGrant{{StartMonths: 1, EveryMonths: 3, AmountCents: 1, ExpiresAfterMonths: 3}}, 12)
	if len(quarterly) != 4 || quarterly[3].MonthOffset != 10 {
		t.Fatalf("quarterly=%+v", quarterly)
	}
}

func TestPlanCreditSummary(t *testing.T) {
	for _, tc := range []struct {
		grants []planGrant
		want   string
	}{
		{nil, "No included credit"},
		{[]planGrant{{EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1}}, "$1,000 every month"},
		{[]planGrant{
			{Repeat: int32p(3), EveryMonths: 1, AmountCents: 3000000, ExpiresAfterMonths: 1},
			{StartMonths: 3, Repeat: int32p(9), EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1},
		}, "3 × $30,000 every month, then 9 × $1,000 every month from month 4"},
		{[]planGrant{
			{Repeat: int32p(1), EveryMonths: 1, AmountCents: 1000000, ExpiresAfterMonths: 3},
			{EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1},
		}, "$10,000 once, each expiring after 3 months, then $1,000 every month"},
		{[]planGrant{{EveryMonths: 1, AmountCents: 12345, ExpiresAfterMonths: 1}}, "$123.45 every month"},
	} {
		if got := planCreditSummary(tc.grants); got != tc.want {
			t.Errorf("summary=%q want %q", got, tc.want)
		}
	}
}

func TestUniformMonthlyCredit(t *testing.T) {
	if c, ok := uniformMonthlyCredit([]planGrant{{EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1}}); !ok || c != 100000 {
		t.Fatalf("uniform=%d %v", c, ok)
	}
	for _, g := range [][]planGrant{
		nil,
		{{Repeat: int32p(12), EveryMonths: 1, AmountCents: 1, ExpiresAfterMonths: 1}},
		{{StartMonths: 1, EveryMonths: 1, AmountCents: 1, ExpiresAfterMonths: 1}},
		{{EveryMonths: 1, AmountCents: 1, ExpiresAfterMonths: 2}},
		{{EveryMonths: 1, AmountCents: 1, ExpiresAfterMonths: 1}, {EveryMonths: 1, AmountCents: 1, ExpiresAfterMonths: 1}},
	} {
		if _, ok := uniformMonthlyCredit(g); ok {
			t.Fatalf("non-uniform schedule accepted: %+v", g)
		}
	}
}

func TestPlanVersionValidate(t *testing.T) {
	valid := planVersion{Family: "pro", DisplayName: "Pro", MonthlyFeeCents: int64p(120000), Limits: planLimits{MaxSandboxes: int32p(1000)},
		CreditSchedule: []planGrant{{EveryMonths: 1, AmountCents: 100000, ExpiresAfterMonths: 1}}, Matching: &planMatching{MatchingBps: 10000, CapCents: 8000000}}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*planVersion){
		"family":                func(v *planVersion) { v.Family = "Pro_1" },
		"name":                  func(v *planVersion) { v.DisplayName = " " },
		"no terms":              func(v *planVersion) { v.MonthlyFeeCents, v.CreditSchedule, v.Matching = nil, nil, nil },
		"fee with underscore":   func(v *planVersion) { v.Family = "custom_example" },
		"unsellable self-serve": func(v *planVersion) { v.SelfServe, v.Public = true, true },
		"no-fee self-serve":     func(v *planVersion) { v.MonthlyFeeCents, v.SelfServe = nil, true },
		"zero fee":              func(v *planVersion) { v.MonthlyFeeCents = int64p(0) },
		"limits":                func(v *planVersion) { v.Limits.MaxSandboxes = nil },
		"matching bps":          func(v *planVersion) { v.Matching.MatchingBps = 10001 },
		"matching cap":          func(v *planVersion) { v.Matching.CapCents = 0 },
		"grant every":           func(v *planVersion) { v.CreditSchedule[0].EveryMonths = 0 },
		"grant repeat":          func(v *planVersion) { v.CreditSchedule[0].Repeat = int32p(0) },
		"display-only terms":    func(v *planVersion) { v.DisplayOnly = true },
		"blank feature":         func(v *planVersion) { v.Features = []string{""} },
	} {
		v := valid
		v.CreditSchedule = append([]planGrant{}, valid.CreditSchedule...)
		m := *valid.Matching
		v.Matching = &m
		mutate(&v)
		if err := v.validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, v)
		}
	}
	enterprise := planVersion{Family: "enterprise", DisplayName: "Enterprise", DisplayOnly: true}
	if err := enterprise.validate(); err != nil {
		t.Fatal(err)
	}
	matchingOnly := planVersion{Family: "custom_example", DisplayName: "Example match", Matching: &planMatching{MatchingBps: 10000, CapCents: 8000000}}
	if err := matchingOnly.validate(); err != nil {
		t.Fatalf("fee-less matching plan: %v", err)
	}
	for name, v := range map[string]planVersion{
		"fee-less without underscore": {Family: "pro", DisplayName: "Pro credits", Matching: matchingOnly.Matching},
		"public fee-less":             {Family: "custom_example", Public: true, DisplayName: "Example", Matching: matchingOnly.Matching},
		"NUL in name":                 {Family: "custom_example", DisplayName: "Ex\x00ample", Matching: matchingOnly.Matching},
	} {
		if err := v.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	sellable := valid
	sellable.SelfServe, sellable.Public, sellable.Matching = true, true, nil
	if err := sellable.validate(); err != nil || !sellable.sellable() {
		t.Fatalf("public self-serve plan: %v", err)
	}
	sellable.Public = false
	if sellable.sellable() {
		t.Fatal("non-public plan sellable")
	}
}

func TestPlanOfferInterval(t *testing.T) {
	for key, want := range map[string]int32{"pro_monthly": 1, "starter_annual": 12, "custom_example_annual": 12, "pro": 0, "pro_weekly": 0, "Pro_monthly": 0, "_monthly": 0, "pro__monthly": 0} {
		_, months, ok := planOfferInterval(key)
		if months != want || ok != (want != 0) {
			t.Errorf("%s: months=%d ok=%v", key, months, ok)
		}
	}
}
