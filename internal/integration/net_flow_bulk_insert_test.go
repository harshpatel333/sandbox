//go:build integration

package integration

import (
	"context"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/superserve-ai/sandbox/internal/db"
)

// The flow writer sends a batch as parallel arrays with ""/0 standing for
// absent fields; the rows must land with those fields NULL.
func TestIntegration_InsertNetFlowsStoresBatchWithNulls(t *testing.T) {
	ctx := context.Background()
	if err := maintainLog(ctx, "net_flow"); err != nil {
		t.Fatalf("maintain: %v", err)
	}
	team, sandbox := uuid.New(), uuid.New()
	if err := testQueries.InsertNetFlows(ctx, db.InsertNetFlowsParams{
		TeamIds: []uuid.UUID{team, team}, SandboxIds: []uuid.UUID{sandbox, sandbox},
		Protocols: []string{"tls", "other"}, Hosts: []string{"api.example.com", ""},
		DstIps:   []netip.Addr{netip.MustParseAddr("192.0.2.4"), netip.MustParseAddr("192.0.2.5")},
		DstPorts: []int32{443, 22}, Verdicts: []string{"allowed", "blocked"}, MatchRules: []string{"domain", ""},
		BytesSent: []int64{100, 0}, BytesRecv: []int64{200, 0}, DurationMs: []int32{12, 0},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := testPool.Query(ctx, `SELECT protocol, host, host(dst_ip), dst_port, verdict, match_rule, bytes_sent, bytes_recv, duration_ms
		FROM net_flow WHERE sandbox_id = $1 AND team_id = $2 ORDER BY dst_port DESC`, sandbox, team)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	type row struct {
		protocol, dstIP, verdict string
		host, matchRule          *string
		dstPort                  int32
		bytesSent, bytesRecv     *int64
		durationMs               *int32
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.protocol, &r.host, &r.dstIP, &r.dstPort, &r.verdict, &r.matchRule, &r.bytesSent, &r.bytesRecv, &r.durationMs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	full, empty := got[0], got[1]
	if full.protocol != "tls" || full.host == nil || *full.host != "api.example.com" || full.dstIP != "192.0.2.4" ||
		full.matchRule == nil || *full.matchRule != "domain" || full.bytesSent == nil || *full.bytesSent != 100 ||
		full.bytesRecv == nil || *full.bytesRecv != 200 || full.durationMs == nil || *full.durationMs != 12 {
		t.Fatalf("full row = %+v", full)
	}
	if empty.verdict != "blocked" || empty.host != nil || empty.matchRule != nil || empty.bytesSent != nil ||
		empty.bytesRecv != nil || empty.durationMs != nil {
		t.Fatalf("empty optionals not NULL: %+v", empty)
	}
}
