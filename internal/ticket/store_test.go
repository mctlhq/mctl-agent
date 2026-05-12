package ticket

import (
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
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

	if err := store.Create(tk); err != nil {
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

	got, err := store.Get(tk.ID)
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

	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(tk.ID)
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
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	tk.Status = StatusAnalyzing
	tk.Analysis = "ArgoCD app is degraded"
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(tk.ID)
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
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	ev := Evidence{Type: "logs", Content: "error: OOMKilled", CollectedAt: time.Now().UTC()}
	if err := store.AddEvidence(tk.ID, ev); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(tk.ID)
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
	dup, err := store.FindDuplicate("billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected nil for no duplicate")
	}

	// Create a ticket.
	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	// Same tenant/service/type → should find duplicate.
	dup, err = store.FindDuplicate("billing", "api", TypePodCrashloop)
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
	dup, err = store.FindDuplicate("billing", "api", TypeResourceLimit)
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
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	dup, err = store.FindDuplicate("billing", "api", TypePodCrashloop)
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
	for _, s := range []string{StatusOpen, StatusAnalyzing, StatusResolved, StatusSuppressed} {
		tk := &Ticket{Source: SourcePolling, Type: TypeArgoCDDegraded, Tenant: "t", Service: "s", Status: s}
		tk.Status = "" // Let Create set default
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
		if s != StatusOpen {
			tk.Status = s
			if err := store.Update(tk); err != nil {
				t.Fatal(err)
			}
		}
	}

	open, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	// Should include open + analyzing but not resolved/suppressed.
	if len(open) != 2 {
		t.Errorf("expected 2 open tickets, got %d", len(open))
	}

	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 total tickets, got %d", len(all))
	}
}

func TestStoreResolveByTenantService(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	ids, err := store.ResolveByTenantService("billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != tk.ID {
		t.Errorf("expected resolved ids = [%s], got %v", tk.ID, ids)
	}

	got, err := store.Get(tk.ID)
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
	if err := store.Create(t1); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(t2); err != nil {
		t.Fatal(err)
	}
	// Already-resolved ticket on the same key must be excluded from results.
	t3 := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api", Status: StatusResolved}
	if err := store.Create(t3); err != nil {
		t.Fatal(err)
	}
	// Ticket on a different service is unrelated and must not be returned.
	other := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "worker"}
	if err := store.Create(other); err != nil {
		t.Fatal(err)
	}

	ids, err := store.ResolveByTenantService("billing", "api", TypePodCrashloop)
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

	ids, err := store.ResolveByTenantService("nope", "nope", TypePodCrashloop)
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
	count, err := store.CountPRsInWindow(1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Create ticket with PR URL.
	tk := &Ticket{Source: SourcePolling, Type: TypePodCrashloop, Tenant: "t", Service: "s", PRURL: "https://github.com/mctlhq/mctl-gitops/pull/1"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	count, err = store.CountPRsInWindow(1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	// Ticket without PR URL should not count.
	tk2 := &Ticket{Source: SourcePolling, Type: TypePodCrashloop, Tenant: "t2", Service: "s2"}
	if err := store.Create(tk2); err != nil {
		t.Fatal(err)
	}

	count, err = store.CountPRsInWindow(1)
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
		if err := store.Create(tk); err != nil {
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
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListByFilters("analyzing", "platform-db", "shared", 100)
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
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	if err := store.TouchWithFingerprint(tk.ID, "fp-B"); err != nil {
		t.Fatalf("touch fp-B: %v", err)
	}
	if err := store.TouchWithFingerprint(tk.ID, "fp-A"); err != nil {
		t.Fatalf("touch fp-A again (dup): %v", err)
	}
	if err := store.TouchWithFingerprint(tk.ID, "fp-C"); err != nil {
		t.Fatalf("touch fp-C: %v", err)
	}

	got, err := store.Get(tk.ID)
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
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	const N = 16
	fps := make([]string, N)
	for i := 0; i < N; i++ {
		fps[i] = "fp-" + string(rune('A'+i))
	}
	for _, fp := range fps {
		if err := store.TouchWithFingerprint(tk.ID, fp); err != nil {
			t.Fatalf("TouchWithFingerprint(%s): %v", fp, err)
		}
	}
	// Idempotency: second pass must not duplicate or drop entries.
	for _, fp := range fps {
		if err := store.TouchWithFingerprint(tk.ID, fp); err != nil {
			t.Fatalf("TouchWithFingerprint repeat(%s): %v", fp, err)
		}
	}

	got, err := store.Get(tk.ID)
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
		if err := store.Create(tk); err != nil {
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
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
		tk.Status = StatusAnalyzing
		if err := store.Update(tk); err != nil {
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
	if err := store.Create(resolved); err != nil {
		t.Fatal(err)
	}
	resolved.Status = StatusResolved
	if err := store.Update(resolved); err != nil {
		t.Fatal(err)
	}

	breakdown, err := store.OpenTicketBreakdown()
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
