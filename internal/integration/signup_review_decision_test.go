//go:build integration

package integration

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegration_SignupReviewDecisionFirstWriterAndPrivileges(t *testing.T) {
	owner := teamCreationDatabase(t, func(pool *pgxpool.Pool) {
		rolloutExec(t, pool, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO anon,authenticated,service_role;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO anon,authenticated,service_role`)
	})
	service := teamCreationServiceRolePool(t, owner)
	user, staff, plan := uuid.New(), uuid.New(), uuid.New()
	insert := `INSERT INTO public.signup_review_decision(user_id,decision,staff_id,plan_version_id,region) VALUES($1,$2,$3,$4,$5) ON CONFLICT(user_id) DO NOTHING`
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			decision := "approved"
			var version any = plan
			var region any = "use"
			if i%2 == 1 {
				decision = "rejected"
				version = nil
				region = nil
			}
			_, err := service.Exec(t.Context(), insert, user, decision, staff, version, region)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent decision", err)
		}
	}
	read := func() string {
		t.Helper()
		var row string
		if err := service.QueryRow(t.Context(), `SELECT to_jsonb(d)::text FROM public.signup_review_decision d WHERE user_id=$1`, user).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := read()
	var count int
	if err := service.QueryRow(t.Context(), `SELECT count(*) FROM public.signup_review_decision WHERE user_id=$1`, user).Scan(&count); err != nil || count != 1 {
		t.Fatalf("decision count %d: %v", count, err)
	}
	if _, err := service.Exec(t.Context(), insert, user, "approved", uuid.New(), uuid.New(), "usw"); err != nil {
		t.Fatal("ignore conflicting replay", err)
	}
	if after := read(); after != before {
		t.Fatal("replay replaced winning decision attribution or terms")
	}
	expectCode := func(pool *pgxpool.Pool, statement, code string, args ...any) {
		t.Helper()
		_, err := pool.Exec(t.Context(), statement, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s expected%s got%v", statement, code, err)
		}
	}
	for _, role := range []string{"anon", "authenticated", "service_role"} {
		for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
			var allowed bool
			if err := owner.QueryRow(t.Context(), `SELECT has_table_privilege($1,'public.signup_review_decision',$2)`, role, privilege).Scan(&allowed); err != nil {
				t.Fatal(err)
			}
			want := role == "service_role" && (privilege == "SELECT" || privilege == "INSERT")
			if allowed != want {
				t.Fatalf("%s %s=%v want%v", role, privilege, allowed, want)
			}
		}
	}
	expectCode(service, `UPDATE public.signup_review_decision SET staff_id=gen_random_uuid() WHERE user_id=$1`, "42501", user)
	expectCode(service, `DELETE FROM public.signup_review_decision WHERE user_id=$1`, "42501", user)
	expectCode(service, `TRUNCATE public.signup_review_decision`, "42501")
	expectCode(service, insert, "23514", uuid.New(), "approved", staff, nil, "use")
	expectCode(service, insert, "23514", uuid.New(), "approved", staff, plan, "")
	expectCode(service, insert, "23514", uuid.New(), "rejected", staff, plan, "use")
	rolloutExec(t, owner, `GRANT ALL ON public.signup_review_decision TO anon,authenticated,service_role`)
	expectCode(service, `UPDATE public.signup_review_decision SET staff_id=gen_random_uuid() WHERE user_id=$1`, "55000", user)
	expectCode(service, `DELETE FROM public.signup_review_decision WHERE user_id=$1`, "55000", user)
	expectCode(service, `TRUNCATE public.signup_review_decision`, "55000")
	for _, role := range []string{"anon", "authenticated"} {
		tx, err := owner.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+role); err != nil {
			t.Fatal(err)
		}
		if err = tx.QueryRow(t.Context(), `SELECT count(*) FROM public.signup_review_decision`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s sees staff decisions: %d %v", role, count, err)
		}
		if _, err = tx.Exec(t.Context(), insert, uuid.New(), "rejected", staff, nil, nil); err == nil {
			t.Fatalf("%s wrote a decision", role)
		}
		_ = tx.Rollback(t.Context())
	}
	if after := read(); after != before {
		t.Fatal("destructive probes changed winning decision")
	}
}
