package ticket

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ctx is the context the store calls in this package's tests run under. The
// store API takes one now; nothing here exercises cancellation, so a single
// background context keeps the call sites readable. Tests that need their own
// context shadow this with a local one.
var ctx = context.Background()

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// A context cancelled before migration starts must abort NewStore promptly
// instead of completing the migration regardless of ctx, the behaviour that
// let a post-connect hang during migrate() ignore SIGTERM (issue #74).
func TestNewStoreAbortsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := NewStore(ctx, ":memory:")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected NewStore to fail when the context is already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("NewStore took %s to observe cancellation, want near-immediate", elapsed)
	}
}

func TestStoreCreateAndGet(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source:    SourceAlertManager,
		AlertName: "PodCrashLooping",
		Type:      TypePodCrashloop,
		Tenant:    "billing",
		Service:   "api",
		Summary:   "Pod is crash looping",
		Severity:  SeverityCritical,
	}

	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	if tk.ID == "" {
		t.Fatal("expected ID to be generated")
	}
	if tk.Status != StatusOpen {
		t.Errorf("expected status open, got %s", tk.Status)
	}
	if tk.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypePodCrashloop {
		t.Errorf("expected type %s, got %s", TypePodCrashloop, got.Type)
	}
	if got.Tenant != "billing" {
		t.Errorf("expected tenant billing, got %s", got.Tenant)
	}
	if got.Service != "api" {
		t.Errorf("expected service api, got %s", got.Service)
	}
	if got.AlertName != "PodCrashLooping" {
		t.Errorf("expected alert name PodCrashLooping, got %s", got.AlertName)
	}
}

func TestStorePersistsPRMetadata(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source:      SourceManual,
		Type:        TypeGeneric,
		Tenant:      "labs",
		Service:     "openclaw",
		Summary:     "test",
		PRURL:       "https://github.com/mctlhq/mctl-gitops/pull/101",
		PRNumber:    101,
		PRRepo:      "mctlhq/mctl-gitops",
		PRBranch:    "openclaw/ticket-101",
		PRCommitSHA: "deadbeef101",
	}

	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRRepo != tk.PRRepo || got.PRBranch != tk.PRBranch || got.PRCommitSHA != tk.PRCommitSHA {
		t.Fatalf("expected PR metadata round-trip, got repo=%s branch=%s sha=%s", got.PRRepo, got.PRBranch, got.PRCommitSHA)
	}
}

func TestStoreUpdate(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourcePolling, Type: TypeArgoCDDegraded, Tenant: "data", Service: "etl"}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	tk.Status = StatusAnalyzing
	tk.Analysis = "ArgoCD app is degraded"
	if err := store.Update(ctx, tk); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAnalyzing {
		t.Errorf("expected status analyzing, got %s", got.Status)
	}
	if got.Analysis != "ArgoCD app is degraded" {
		t.Errorf("unexpected analysis: %s", got.Analysis)
	}
}

func TestStoreAddEvidence(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "t", Service: "s"}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	ev := Evidence{Type: "logs", Content: "error: OOMKilled", CollectedAt: time.Now().UTC()}
	if err := store.AddEvidence(ctx, tk.ID, ev); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 1 {
		t.Fatalf("expected 1 evidence, got %d", len(got.Evidence))
	}
	if got.Evidence[0].Type != "logs" {
		t.Errorf("expected evidence type logs, got %s", got.Evidence[0].Type)
	}
	if got.Evidence[0].Content != "error: OOMKilled" {
		t.Errorf("unexpected evidence content: %s", got.Evidence[0].Content)
	}
}

func TestStoreFindDuplicate(t *testing.T) {
	store := newTestStore(t)

	// No tickets yet — should return nil.
	dup, err := store.FindDuplicate(ctx, "billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected nil for no duplicate")
	}

	// Create a ticket.
	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	// Same tenant/service/type → should find duplicate.
	dup, err = store.FindDuplicate(ctx, "billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup == nil {
		t.Fatal("expected duplicate to be found")
	}
	if dup.ID != tk.ID {
		t.Errorf("expected dup ID %s, got %s", tk.ID, dup.ID)
	}

	// Different type → no duplicate.
	dup, err = store.FindDuplicate(ctx, "billing", "api", TypeResourceLimit)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected no duplicate for different type")
	}

	// Resolved ticket → no duplicate.
	tk.Status = StatusResolved
	now := time.Now().UTC()
	tk.ResolvedAt = &now
	if err := store.Update(ctx, tk); err != nil {
		t.Fatal(err)
	}
	dup, err = store.FindDuplicate(ctx, "billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected no duplicate for resolved ticket")
	}
}

func TestStoreListOpenAndListAll(t *testing.T) {
	store := newTestStore(t)

	// Create several tickets with different statuses.
	for _, s := range []string{StatusOpen, StatusAnalyzing, StatusEscalated, StatusResolved, StatusSuppressed} {
		tk := &Ticket{Source: SourcePolling, Type: TypeArgoCDDegraded, Tenant: "t", Service: "s", Status: s}
		tk.Status = "" // Let Create set default
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
		if s != StatusOpen {
			tk.Status = s
			if err := store.Update(ctx, tk); err != nil {
				t.Fatal(err)
			}
		}
	}

	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Should include open + analyzing + escalated but not resolved/suppressed.
	// Escalated is terminal for the pipeline but the underlying problem is
	// still live, so the watchdog and reconcile loops must keep seeing it.
	if len(open) != 3 {
		t.Errorf("expected 3 open tickets, got %d", len(open))
	}
	var sawEscalated bool
	for _, tk := range open {
		if tk.Status == StatusEscalated {
			sawEscalated = true
		}
	}
	if !sawEscalated {
		t.Error("ListOpen must include escalated tickets, otherwise they are never GC'd")
	}

	all, err := store.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("expected 5 total tickets, got %d", len(all))
	}
}

func TestStoreResolveByTenantService(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	ids, err := store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != tk.ID {
		t.Errorf("expected resolved ids = [%s], got %v", tk.ID, ids)
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusResolved {
		t.Errorf("expected resolved, got %s", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Error("expected ResolvedAt to be set")
	}
}

func TestStoreResolveByTenantServiceReturnsAllOpenIDs(t *testing.T) {
	store := newTestStore(t)

	// Two open tickets with the same (tenant, service, type) — fan-out
	// to mctl-api needs every open ID, not just the most recent.
	t1 := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	t2 := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, t2); err != nil {
		t.Fatal(err)
	}
	// Already-resolved ticket on the same key must be excluded from results.
	t3 := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api", Status: StatusResolved}
	if err := store.Create(ctx, t3); err != nil {
		t.Fatal(err)
	}
	// Ticket on a different service is unrelated and must not be returned.
	other := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "worker"}
	if err := store.Create(ctx, other); err != nil {
		t.Fatal(err)
	}

	ids, err := store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d (%v)", len(ids), ids)
	}
	got := map[string]bool{ids[0]: true, ids[1]: true}
	if !got[t1.ID] || !got[t2.ID] {
		t.Errorf("expected ids to include %s and %s, got %v", t1.ID, t2.ID, ids)
	}
	if got[t3.ID] {
		t.Errorf("already-resolved ticket %s should not be in returned ids", t3.ID)
	}
	if got[other.ID] {
		t.Errorf("unrelated ticket %s should not be in returned ids", other.ID)
	}
}

func TestStoreResolveByTenantServiceNoMatch(t *testing.T) {
	store := newTestStore(t)

	ids, err := store.ResolveByTenantService(ctx, "nope", "nope", TypePodCrashloop, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty ids when no rows match, got %v", ids)
	}
}

func TestStoreCountPRsInWindow(t *testing.T) {
	store := newTestStore(t)

	// No tickets → count = 0.
	count, err := store.CountPRsInWindow(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Create ticket with PR URL.
	tk := &Ticket{Source: SourcePolling, Type: TypePodCrashloop, Tenant: "t", Service: "s", PRURL: "https://github.com/mctlhq/mctl-gitops/pull/1"}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	count, err = store.CountPRsInWindow(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	// Ticket without PR URL should not count.
	tk2 := &Ticket{Source: SourcePolling, Type: TypePodCrashloop, Tenant: "t2", Service: "s2"}
	if err := store.Create(ctx, tk2); err != nil {
		t.Fatal(err)
	}

	count, err = store.CountPRsInWindow(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 (only PR ticket), got %d", count)
	}
}

func TestListByFiltersSurviveTableCap(t *testing.T) {
	// Regression: SQL-side filtering must happen BEFORE LIMIT, otherwise
	// a narrow query (e.g. tenant=X AND service=Y) silently drops matches
	// that fall outside the latest 100 rows and looks like "none found".
	//
	// The matching rows MUST be older than the noise rows — otherwise
	// they land inside the latest-100 window and a buggy "LIMIT then
	// filter" implementation would still pass this test.
	store := newTestStore(t)

	// 5 matching tickets, inserted first (oldest).
	for i := 0; i < 5; i++ {
		tk := &Ticket{
			Source:  SourceAlertManager,
			Type:    TypeResourceLimit,
			Tenant:  "platform-db",
			Service: "shared",
			Status:  StatusAnalyzing,
		}
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// Ensure strictly monotonically newer timestamps on the noise batch
	// so ORDER BY created_at DESC puts the 150 noise rows ahead of the
	// 5 matching ones. SQLite's DATETIME has microsecond precision; a
	// small sleep is enough.
	time.Sleep(10 * time.Millisecond)
	// 150 noise tickets for a different tenant/service (newer).
	for i := 0; i < 150; i++ {
		tk := &Ticket{
			Source:  SourceAlertManager,
			Type:    TypeResourceLimit,
			Tenant:  "other",
			Service: "svc",
			Status:  StatusSuppressed,
		}
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListByFilters(context.Background(), "analyzing", "platform-db", "shared", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("expected all 5 analyzing platform-db/shared tickets, got %d", len(got))
	}
	for _, tk := range got {
		if tk.Status != StatusAnalyzing || tk.Tenant != "platform-db" || tk.Service != "shared" {
			t.Errorf("unexpected ticket in result: %+v", tk)
		}
	}
}

func TestEvidenceJSON(t *testing.T) {
	data := map[string]string{"health": "Degraded"}
	got := EvidenceJSON(data)
	if got != `{"health":"Degraded"}` {
		t.Errorf("unexpected JSON: %s", got)
	}
}

// TestTouchWithFingerprintMergesNotOverwrites guards the Codex P1 fix
// on PR #13. Tickets are deduplicated by (tenant, service, type), so
// duplicate-touch from a second AlertManager alert with a different
// fingerprint must accumulate the fingerprint into a CSV set rather
// than overwrite. The reconciliation pass downstream only resolves the
// ticket when ALL fingerprints are absent from AM.
func TestTouchWithFingerprintMergesNotOverwrites(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source:           SourceAlertManager,
		Type:             TypePodCrashloop,
		Tenant:           "labs",
		Service:          "svc",
		Summary:          "first alert",
		Severity:         SeverityCritical,
		AlertFingerprint: "fp-A",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	if err := store.TouchWithFingerprint(ctx, tk.ID, "fp-B"); err != nil {
		t.Fatalf("touch fp-B: %v", err)
	}
	if err := store.TouchWithFingerprint(ctx, tk.ID, "fp-A"); err != nil {
		t.Fatalf("touch fp-A again (dup): %v", err)
	}
	if err := store.TouchWithFingerprint(ctx, tk.ID, "fp-C"); err != nil {
		t.Fatalf("touch fp-C: %v", err)
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AlertFingerprint != "fp-A,fp-B,fp-C" {
		t.Errorf("expected merged CSV 'fp-A,fp-B,fp-C', got %q", got.AlertFingerprint)
	}
}

// TestTouchWithFingerprintRepeatedSequential exercises the atomic
// CASE-expression merge under repeated calls. Sequential order — pure
// goroutine concurrency hits SQLite's per-DB-file write lock, which
// is a separate (pre-existing) operational concern. The atomicity of
// the merge itself is provable by inspection: the new value is
// computed from the existing column inside a single UPDATE statement,
// so no read/modify/write window exists at the application layer.
func TestTouchWithFingerprintRepeatedSequential(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source:           SourceAlertManager,
		Type:             TypePodCrashloop,
		Tenant:           "labs",
		Service:          "svc",
		Summary:          "many-fp",
		Severity:         SeverityCritical,
		AlertFingerprint: "",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	const N = 16
	fps := make([]string, N)
	for i := 0; i < N; i++ {
		fps[i] = "fp-" + string(rune('A'+i))
	}
	for _, fp := range fps {
		if err := store.TouchWithFingerprint(ctx, tk.ID, fp); err != nil {
			t.Fatalf("TouchWithFingerprint(%s): %v", fp, err)
		}
	}
	// Idempotency: second pass must not duplicate or drop entries.
	for _, fp := range fps {
		if err := store.TouchWithFingerprint(ctx, tk.ID, fp); err != nil {
			t.Fatalf("TouchWithFingerprint repeat(%s): %v", fp, err)
		}
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	have := strings.Split(got.AlertFingerprint, ",")
	if len(have) != N {
		t.Fatalf("expected %d unique fingerprints, got %d in %q",
			N, len(have), got.AlertFingerprint)
	}
	seen := map[string]bool{}
	for _, fp := range have {
		if seen[fp] {
			t.Errorf("duplicate fingerprint in result: %q", fp)
		}
		seen[fp] = true
	}
	for _, want := range fps {
		if !seen[want] {
			t.Errorf("fingerprint %q missing from set; got %q", want, got.AlertFingerprint)
		}
	}
}

func TestMergeFingerprintHelper(t *testing.T) {
	cases := []struct {
		existing, fp, want string
	}{
		{"", "", ""},
		{"", "fp-A", "fp-A"},
		{"fp-A", "", "fp-A"},
		{"fp-A", "fp-A", "fp-A"},
		{"fp-A", "fp-B", "fp-A,fp-B"},
		{"fp-A,fp-B", "fp-A", "fp-A,fp-B"},
		{"fp-A,fp-B", "fp-C", "fp-A,fp-B,fp-C"},
	}
	for _, tc := range cases {
		got := mergeFingerprint(tc.existing, tc.fp)
		if got != tc.want {
			t.Errorf("mergeFingerprint(%q, %q) = %q; want %q",
				tc.existing, tc.fp, got, tc.want)
		}
	}
}

func TestStoreOpenTicketBreakdown(t *testing.T) {
	store := newTestStore(t)

	// 3 open + alertmanager tickets
	for i := 0; i < 3; i++ {
		tk := &Ticket{
			Source:   SourceAlertManager,
			Type:     TypePodCrashloop,
			Tenant:   "labs",
			Service:  "svc",
			Summary:  "open alert",
			Severity: SeverityCritical,
		}
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// 2 analyzing + alertmanager tickets
	for i := 0; i < 2; i++ {
		tk := &Ticket{
			Source:   SourceAlertManager,
			Type:     TypeResourceLimit,
			Tenant:   "labs",
			Service:  "svc2",
			Summary:  "analyzing alert",
			Severity: SeverityWarning,
		}
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
		tk.Status = StatusAnalyzing
		if err := store.Update(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// 1 resolved ticket — must be excluded
	resolved := &Ticket{
		Source:   SourceAlertManager,
		Type:     TypeGeneric,
		Tenant:   "labs",
		Service:  "svc3",
		Summary:  "done",
		Severity: SeverityInfo,
	}
	if err := store.Create(ctx, resolved); err != nil {
		t.Fatal(err)
	}
	resolved.Status = StatusResolved
	if err := store.Update(ctx, resolved); err != nil {
		t.Fatal(err)
	}

	breakdown, err := store.OpenTicketBreakdown(ctx)
	if err != nil {
		t.Fatalf("OpenTicketBreakdown: %v", err)
	}
	if len(breakdown) != 2 {
		t.Fatalf("expected 2 pairs, got %d: %v", len(breakdown), breakdown)
	}

	openKey := StatusSourcePair{Status: StatusOpen, Source: SourceAlertManager}
	analyzingKey := StatusSourcePair{Status: StatusAnalyzing, Source: SourceAlertManager}

	if got := breakdown[openKey]; got != 3 {
		t.Errorf("open/alertmanager count = %d, want 3", got)
	}
	if got := breakdown[analyzingKey]; got != 2 {
		t.Errorf("analyzing/alertmanager count = %d, want 2", got)
	}
	// resolved ticket must not appear
	for k := range breakdown {
		if k.Status == StatusResolved {
			t.Errorf("resolved tickets must be excluded from breakdown; found key %+v", k)
		}
	}
}

// NewStore is called in a retry loop, so a failed migration must not leave the
// *sql.DB (and its background connectionOpener goroutine) behind.
func TestNewStoreClosesDBWhenMigrationFails(t *testing.T) {
	// A path under a directory that does not exist: sql.Open succeeds (it is
	// lazy), the first migrate statement fails.
	bad := filepath.Join(t.TempDir(), "no-such-dir", "agent.db")

	before := runtime.NumGoroutine()
	if _, err := NewStore(context.Background(), bad); err == nil {
		t.Fatal("expected NewStore to fail on an unwritable path")
	}

	// The opener goroutine exits asynchronously after Close, so allow it a
	// moment rather than sampling the instant after the call.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutine leaked after a failed migration: %d before, %d after", before, after)
	}
}

// A ticket that deduped several concurrent alerts carries all of their
// fingerprints, and a resolve on any single one of them must still close it.
// Every other test of the fingerprint scope uses a ticket holding exactly one
// fingerprint, where membership and equality are indistinguishable — so a
// regression that swapped the padded LIKE for `alert_fingerprint = ?` would
// pass all of them. This is the test that fails.
func TestStoreResolveByTenantServiceMatchesFingerprintSetMembership(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source: "alertmanager", Type: TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
		AlertFingerprint: "fp-A",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{"fp-B", "fp-C"} {
		if err := store.TouchWithFingerprint(ctx, tk.ID, fp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AlertFingerprint != "fp-A,fp-B,fp-C" {
		t.Fatalf("precondition: want merged set fp-A,fp-B,fp-C, got %q", got.AlertFingerprint)
	}

	// A near-miss substring of a real element must NOT match: without the
	// comma padding, "p-B" is a substring of ",fp-A,fp-B,fp-C," and would.
	ids, err := store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "p-B", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("substring %q must not match an element of the set, resolved %v", "p-B", ids)
	}

	// The middle element must match — not just the first or the last, which
	// a half-broken padding could still get right by accident.
	ids, err = store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "fp-B", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != tk.ID {
		t.Fatalf("resolving on set member fp-B: want [%s], got %v", tk.ID, ids)
	}
}

// The fingerprint is copied verbatim out of the webhook body and the webhook's
// bearer token is optional, so LIKE metacharacters in it must be inert data.
// Unescaped, "%" is a wildcard that resolves every ticket under the key.
func TestStoreResolveByTenantServiceTreatsLikeWildcardsAsLiterals(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source: "alertmanager", Type: TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
		AlertFingerprint: "fp-A",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	for _, fp := range []string{"%", "fp_A", "%A"} {
		ids, err := store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, fp, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Fatalf("fingerprint %q must be matched literally, not as a pattern; resolved %v", fp, ids)
		}
	}
}

// notAfter is the occurrence boundary: an AlertManager fingerprint is stable
// across flaps, so a redelivered resolve carries the same fingerprint as the
// alert's next occurrence. A ticket opened after the alert already ended
// belongs to that next occurrence and must survive the replay.
func TestStoreResolveByTenantServiceSparesTicketsOpenedAfterTheAlertEnded(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source: "alertmanager", Type: TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
		AlertFingerprint: "fp-A",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	endedBeforeTicket := tk.CreatedAt.Add(-time.Minute)
	ids, err := store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "fp-A", endedBeforeTicket)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("ticket created after the alert ended must not be resolved by it, got %v", ids)
	}

	endedAfterTicket := tk.CreatedAt.Add(time.Minute)
	ids, err = store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "fp-A", endedAfterTicket)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != tk.ID {
		t.Fatalf("ticket predating the alert's end must resolve: want [%s], got %v", tk.ID, ids)
	}
}

// Fingerprints have not always been persisted. An open ticket predating that
// carries an empty set, and reconcileWithAlertManager skips exactly those
// rows — so if the fingerprint scope refused to match them, they and their
// mctl-api incidents would stay open until TTL GC.
func TestStoreResolveByTenantServiceStillResolvesFingerprintlessLegacyTickets(t *testing.T) {
	store := newTestStore(t)

	legacy := &Ticket{
		Source: "alertmanager", Type: TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
	}
	if err := store.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.AlertFingerprint != "" {
		t.Fatalf("precondition: want an empty fingerprint set, got %q", legacy.AlertFingerprint)
	}

	ids, err := store.ResolveByTenantService(ctx, "billing", "api", TypePodCrashloop, "fp-A", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != legacy.ID {
		t.Fatalf("legacy fingerprintless ticket must still resolve: want [%s], got %v", legacy.ID, ids)
	}
}

// Every wrapper that forwards to another Store method must forward the
// context too. ListAll shipped in this refactor's first commit taking a ctx
// and then calling ListByFilters(context.Background(), ...) — the parameter
// was accepted and silently dropped, so its one caller could never cancel the
// query it had asked to be cancellable.
func TestStoreListAllHonoursItsContext(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source: "alertmanager", Type: TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.ListAll(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListAll must use the context it is given: want context.Canceled, got %v", err)
	}
}
