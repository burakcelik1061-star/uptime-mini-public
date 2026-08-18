package incidents

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu paketin testleri GERÇEK VERİTABANINA karşı koşuyor (1.2'nin kararı):
// 4.1'in asıl garantisi ("aynı kesinti için ikinci incident açılmaz") bir
// kısmi unique indeks ve `ON CONFLICT DO NOTHING` — sahte bir store ile
// ölçülen şey benim yazdığım sahtenin davranışı olurdu. Kesinti başlangıcının
// ham kontrol satırlarından türetilmesi de aynı sebeple burada.

func newService(t *testing.T, pool *pgxpool.Pool) (*Service, *store.Queries) {
	t.Helper()

	queries := store.New(pool)

	return NewService(pool, queries, slog.New(slog.DiscardHandler)), queries
}

func createOrg(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()

	queries := store.New(pool)

	user, err := queries.CreateUser(t.Context(), store.CreateUserParams{
		Email:        "uye-" + uuid.NewString() + "@example.com",
		PasswordHash: "$argon2id$test",
		Name:         "Test Kullanıcı",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	org, err := queries.CreateOrganization(t.Context(), store.CreateOrganizationParams{
		Name: "Test Org",
		Slug: "org-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("organizasyon oluşturulamadı: %v", err)
	}

	if _, err := queries.AddOrganizationMember(t.Context(), store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: user.ID,
		Role:   store.OrgRoleOwner,
	}); err != nil {
		t.Fatalf("üyelik eklenemedi: %v", err)
	}

	return org.ID
}

func createMonitor(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID) store.Monitor {
	t.Helper()

	monitor, err := store.New(pool).CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                orgID,
		Name:                 "Ödeme API",
		Url:                  "https://example.com/health",
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: 60,
		TimeoutMs:            10000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		FailureThreshold:     3,
		RecoveryThreshold:    2,
		IsActive:             true,
		CurrentStatus:        store.MonitorStatusUp,
	})
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	return monitor
}

// recordCheck, zaman serisine tek bir ZAMANLANMIŞ kontrol yazar.
func recordCheck(
	t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID, at time.Time, outcome store.CheckOutcome,
) {
	t.Helper()

	params := store.InsertCheckResultParams{
		MonitorID:   monitorID,
		CheckedAt:   at,
		Outcome:     outcome,
		TriggeredBy: store.CheckTriggerScheduled,
	}

	status := int32(200)
	responseMs := int32(30)

	if outcome == store.CheckOutcomeFailure {
		params.ErrorType = failureType()
	} else {
		params.HttpStatus = &status
		params.ResponseTimeMs = &responseMs
	}

	if _, err := store.New(pool).InsertCheckResult(t.Context(), params); err != nil {
		t.Fatalf("kontrol sonucu yazılamadı: %v", err)
	}
}

// failureType, şemanın "failure → hata tipi zorunlu" kısıtını karşılar (2.3).
func failureType() store.NullCheckErrorType {
	return store.NullCheckErrorType{CheckErrorType: store.CheckErrorTypeTimeout, Valid: true}
}

// downTransition, "eşik doldu, monitor DOWN" geçişini üretir.
func downTransition(failures int32) monitors.Transition {
	return monitors.Transition{
		From: store.MonitorStatusUp,
		Next: monitors.State{
			Status:              store.MonitorStatusDown,
			ConsecutiveFailures: failures,
		},
		Changed: true,
	}
}

// upTransition, "kurtarma eşiği doldu, monitor UP" geçişini üretir.
func upTransition(successes int32) monitors.Transition {
	return monitors.Transition{
		From: store.MonitorStatusDown,
		Next: monitors.State{
			Status:               store.MonitorStatusUp,
			ConsecutiveSuccesses: successes,
		},
		Changed: true,
	}
}

// KESİNTİNİN BAŞLANGICI, EŞİĞİN DOLDUĞU AN DEĞİL: 3 eşikli bir monitor'de
// incident, ilk başarısız kontrolün zamanıyla açılıyor — yoksa her kesinti
// sistematik olarak iki kontrol aralığı kadar kısa görünürdü.
func TestApplyTransitionOpensIncidentAtFirstFailure(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	recordCheck(t, pool, monitor.ID, base.Add(-time.Minute), store.CheckOutcomeSuccess)

	firstFailure := base
	recordCheck(t, pool, monitor.ID, firstFailure, store.CheckOutcomeFailure)
	recordCheck(t, pool, monitor.ID, base.Add(time.Minute), store.CheckOutcomeFailure)

	detectedAt := base.Add(2 * time.Minute)
	recordCheck(t, pool, monitor.ID, detectedAt, store.CheckOutcomeFailure)

	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(3), detectedAt)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionOpened {
		t.Fatalf("action = %q, beklenen %q", action, ActionOpened)
	}

	incident := openIncidentFor(t, pool, monitor.ID)

	if !incident.StartedAt.Equal(firstFailure) {
		t.Errorf("started_at = %v, beklenen %v (serinin ilk hatası)", incident.StartedAt, firstFailure)
	}
	if incident.ResolvedAt != nil {
		t.Errorf("yeni incident kapalı geldi: %v", incident.ResolvedAt)
	}
	if !incident.IsAutoCreated {
		t.Error("is_auto_created = false, beklenen true")
	}
	if incident.Status != store.IncidentStatusInvestigating {
		t.Errorf("status = %q, beklenen investigating", incident.Status)
	}
	if incident.Impact != AutoImpact {
		t.Errorf("impact = %q, beklenen %q", incident.Impact, AutoImpact)
	}
	if incident.Title != AutoTitle(monitor.Name) {
		t.Errorf("başlık = %q, beklenen %q", incident.Title, AutoTitle(monitor.Name))
	}
	if incident.OrgID != orgID {
		t.Errorf("org_id = %v, beklenen %v (monitor'den türemeli)", incident.OrgID, orgID)
	}

	// Bağ satırı AYNI ifadede yazılıyor: hiçbir servise bağlı olmayan bir
	// otomatik incident kalmamalı.
	if got := linkedMonitors(t, pool, incident.ID); len(got) != 1 || got[0] != monitor.ID {
		t.Errorf("incident_monitors = %v, beklenen [%v]", got, monitor.ID)
	}
}

// 4.1'İN ASIL GARANTİSİ: aynı kesinti ikinci bir incident açmıyor.
func TestApplyTransitionDoesNotOpenSecondIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))
	at := time.Now().UTC().Add(-time.Minute)
	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)

	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at); err != nil {
		t.Fatalf("ilk açma başarısız: %v", err)
	}

	// Aynı geçiş bir kez daha (ör. durum yazması yeniden denendi, iki worker
	// kopyası aynı anda işledi).
	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at.Add(time.Minute))
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionAlreadyOpen {
		t.Fatalf("action = %q, beklenen %q", action, ActionAlreadyOpen)
	}

	if got := countIncidents(t, pool, monitor.ID); got != 1 {
		t.Fatalf("incident sayısı = %d, beklenen 1", got)
	}
}

// GARANTİ ŞEMADA DA YAZILI: uygulama mantığı atlansa bile veritabanı ikinci
// bir açık otomatik incident'ı reddediyor (projenin "son hakem veritabanı"
// disiplini).
func TestSchemaRejectsSecondOpenAutoIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	insert := `INSERT INTO incidents (org_id, title, impact, auto_monitor_id, is_auto_created)
	           VALUES ($1, $2, 'major', $3, true)`

	if _, err := pool.Exec(t.Context(), insert, orgID, "birinci", monitor.ID); err != nil {
		t.Fatalf("ilk incident yazılamadı: %v", err)
	}

	_, err := pool.Exec(t.Context(), insert, orgID, "ikinci", monitor.ID)
	if err == nil {
		t.Fatal("ikinci açık otomatik incident kabul edildi, beklenen: unique ihlali")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("hata = %v, beklenen 23505 (unique_violation)", err)
	}
}

// Kapanış anı da eşiğin dolduğu an değil: KURTARMA SERİSİNİN İLK başarılı
// kontrolü — servis o anda cevap vermeye başlamıştı.
func TestApplyTransitionResolvesAtFirstSuccess(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))

	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	recordCheck(t, pool, monitor.ID, base, store.CheckOutcomeFailure)

	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), base); err != nil {
		t.Fatalf("incident açılamadı: %v", err)
	}

	firstSuccess := base.Add(time.Minute)
	recordCheck(t, pool, monitor.ID, firstSuccess, store.CheckOutcomeSuccess)

	recoveredAt := base.Add(2 * time.Minute)
	recordCheck(t, pool, monitor.ID, recoveredAt, store.CheckOutcomeSuccess)

	action, err := svc.ApplyTransition(t.Context(), monitor, upTransition(2), recoveredAt)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionResolved {
		t.Fatalf("action = %q, beklenen %q", action, ActionResolved)
	}

	incidents := listIncidents(t, pool, monitor.ID)
	if len(incidents) != 1 {
		t.Fatalf("incident sayısı = %d, beklenen 1", len(incidents))
	}

	incident := incidents[0]
	if incident.ResolvedAt == nil {
		t.Fatal("resolved_at yazılmadı")
	}
	if !incident.ResolvedAt.Equal(firstSuccess) {
		t.Errorf("resolved_at = %v, beklenen %v (kurtarma serisinin ilk başarısı)",
			*incident.ResolvedAt, firstSuccess)
	}
	if incident.Status != store.IncidentStatusResolved {
		t.Errorf("status = %q, beklenen resolved", incident.Status)
	}
	if got := incident.ResolvedAt.Sub(incident.StartedAt); got != time.Minute {
		t.Errorf("kesinti süresi = %v, beklenen 1m", got)
	}
}

// Kapanmış incident kısmi indekste yer tutmuyor: aynı monitor tekrar düşerse
// YENİ bir incident açılıyor.
func TestApplyTransitionOpensNewIncidentAfterResolve(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))
	base := time.Now().UTC().Add(-30 * time.Minute)

	recordCheck(t, pool, monitor.ID, base, store.CheckOutcomeFailure)
	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), base); err != nil {
		t.Fatalf("ilk incident açılamadı: %v", err)
	}

	recovered := base.Add(5 * time.Minute)
	recordCheck(t, pool, monitor.ID, recovered, store.CheckOutcomeSuccess)
	if _, err := svc.ApplyTransition(t.Context(), monitor, upTransition(1), recovered); err != nil {
		t.Fatalf("incident kapatılamadı: %v", err)
	}

	again := base.Add(10 * time.Minute)
	recordCheck(t, pool, monitor.ID, again, store.CheckOutcomeFailure)

	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), again)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionOpened {
		t.Fatalf("action = %q, beklenen %q", action, ActionOpened)
	}

	if got := countIncidents(t, pool, monitor.ID); got != 2 {
		t.Fatalf("incident sayısı = %d, beklenen 2", got)
	}
}

// Kapatılacak açık incident yoksa (elle kapatılmış olabilir) hata değil.
func TestApplyTransitionWithoutOpenIncident(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))
	at := time.Now().UTC()

	action, err := svc.ApplyTransition(t.Context(), monitor, upTransition(2), at)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionNoOpenIncident {
		t.Fatalf("action = %q, beklenen %q", action, ActionNoOpenIncident)
	}
}

// Ham veri yoksa (3.4 budaması) başlangıç TESPİT ANINA düşüyor: incident'sız
// kalmaktansa birkaç dakika kısa görünmesi.
func TestApplyTransitionFallsBackToDetectionTime(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))
	detectedAt := time.Now().UTC().Truncate(time.Second)

	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(3), detectedAt); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	incident := openIncidentFor(t, pool, monitor.ID)
	if !incident.StartedAt.Equal(detectedAt) {
		t.Errorf("started_at = %v, beklenen %v", incident.StartedAt, detectedAt)
	}
}

// MANUEL KONTROLLER SERİYE GİRMİYOR (2.3'ün kararının devamı): "şimdi kontrol
// et" düğmesine basmak kesintinin başlangıcını kaydırmamalı.
func TestApplyTransitionIgnoresManualChecks(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, queries := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)

	firstFailure := base
	recordCheck(t, pool, monitor.ID, firstFailure, store.CheckOutcomeFailure)

	// Araya sıkışan manuel kontrol: seri sorgusu onu görmemeli.
	if _, err := queries.InsertCheckResult(t.Context(), store.InsertCheckResultParams{
		MonitorID:   monitor.ID,
		CheckedAt:   base.Add(30 * time.Second),
		Outcome:     store.CheckOutcomeFailure,
		TriggeredBy: store.CheckTriggerManual,
		ErrorType:   failureType(),
	}); err != nil {
		t.Fatalf("manuel kontrol yazılamadı: %v", err)
	}

	detectedAt := base.Add(time.Minute)
	recordCheck(t, pool, monitor.ID, detectedAt, store.CheckOutcomeFailure)

	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(2), detectedAt); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	incident := openIncidentFor(t, pool, monitor.ID)
	if !incident.StartedAt.Equal(firstFailure) {
		t.Errorf("started_at = %v, beklenen %v", incident.StartedAt, firstFailure)
	}
}

// MONITOR SİLİNSE BİLE INCIDENT SİLİNMİYOR (0.1 §7): geçmiş kesinti status
// page geçmişinde kalmalı. Bağ satırı düşüyor, özne NULL'a iniyor, ama kaydın
// otomatik doğduğu bilgisi (is_auto_created) korunuyor.
func TestIncidentSurvivesMonitorDeletion(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, queries := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	at := time.Now().UTC().Add(-time.Minute)

	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)
	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at); err != nil {
		t.Fatalf("incident açılamadı: %v", err)
	}

	incident := openIncidentFor(t, pool, monitor.ID)

	if _, err := queries.DeleteMonitor(t.Context(), store.DeleteMonitorParams{
		ID: monitor.ID, OrgID: orgID,
	}); err != nil {
		t.Fatalf("monitor silinemedi: %v", err)
	}

	var (
		autoMonitorID *uuid.UUID
		isAuto        bool
		links         int
	)

	if err := pool.QueryRow(t.Context(), `
		SELECT i.auto_monitor_id, i.is_auto_created,
		       (SELECT count(*) FROM incident_monitors im WHERE im.incident_id = i.id)
		FROM incidents i WHERE i.id = $1`, incident.ID,
	).Scan(&autoMonitorID, &isAuto, &links); err != nil {
		t.Fatalf("incident silinmiş olmamalı: %v", err)
	}

	if autoMonitorID != nil {
		t.Errorf("auto_monitor_id = %v, beklenen NULL", *autoMonitorID)
	}
	if !isAuto {
		t.Error("is_auto_created = false, beklenen true (kaydın kökeni korunmalı)")
	}
	if links != 0 {
		t.Errorf("incident_monitors satırı = %d, beklenen 0", links)
	}
}

// Kısmi unique indeks MANUEL incident'ları kısıtlamıyor: bir org aynı anda
// istediği kadar manuel incident açabilir (auto_monitor_id NULL).
func TestSchemaAllowsMultipleOpenManualIncidents(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	orgID := createOrg(t, pool)

	insert := `INSERT INTO incidents (org_id, title, impact, is_auto_created) VALUES ($1, $2, 'minor', false)`

	for _, title := range []string{"bakım", "kısmi kesinti"} {
		if _, err := pool.Exec(t.Context(), insert, orgID, title); err != nil {
			t.Fatalf("manuel incident yazılamadı (%s): %v", title, err)
		}
	}
}

// "Çözüldü ama kapanış zamanı yok" gibi anlamsız satırlar şemaya giremiyor.
func TestSchemaRejectsInconsistentResolution(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	orgID := createOrg(t, pool)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "resolved ama resolved_at yok",
			sql: `INSERT INTO incidents (org_id, title, status, impact, is_auto_created)
			      VALUES ($1, 'x', 'resolved', 'minor', false)`,
			args: []any{orgID},
		},
		{
			name: "resolved_at var ama status açık",
			sql: `INSERT INTO incidents (org_id, title, status, impact, is_auto_created, resolved_at)
			      VALUES ($1, 'x', 'investigating', 'minor', false, now())`,
			args: []any{orgID},
		},
		{
			name: "kapanış başlangıçtan önce",
			sql: `INSERT INTO incidents (org_id, title, status, impact, is_auto_created, started_at, resolved_at)
			      VALUES ($1, 'x', 'resolved', 'minor', false, now(), now() - interval '1 hour')`,
			args: []any{orgID},
		},
		{
			name: "otomatik özne var ama manuel işaretli",
			sql: `INSERT INTO incidents (org_id, title, impact, is_auto_created, auto_monitor_id)
			      VALUES ($1, 'x', 'minor', false, $2)`,
			args: []any{orgID, createMonitor(t, pool, orgID).ID},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), tc.sql, tc.args...); err == nil {
				t.Fatal("satır kabul edildi, beklenen: kısıt ihlali")
			}
		})
	}
}

// Durum DEĞİŞMEDİYSE defter hiç yazmıyor — kararın tek sahibi 3.3'ün saf
// fonksiyonu; kural burada ikinci kez uygulanmıyor.
func TestApplyTransitionIgnoresUnchangedAndNonOutageTransitions(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	monitor := createMonitor(t, pool, createOrg(t, pool))
	at := time.Now().UTC()

	cases := []struct {
		name       string
		transition monitors.Transition
	}{
		{
			name: "değişmedi",
			transition: monitors.Transition{
				From: store.MonitorStatusDown,
				Next: monitors.State{Status: store.MonitorStatusDown, ConsecutiveFailures: 5},
			},
		},
		{
			// DEGRADED kesinti DEĞİL (3.3'ün asimetrisi + 0.1'in uptime formülü
			// degraded'ı "up" sayıyor).
			name: "up → degraded",
			transition: monitors.Transition{
				From:    store.MonitorStatusUp,
				Next:    monitors.State{Status: store.MonitorStatusDegraded},
				Changed: true,
			},
		},
		{
			name: "pending → up",
			transition: monitors.Transition{
				From:    store.MonitorStatusPending,
				Next:    monitors.State{Status: store.MonitorStatusUp},
				Changed: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, err := svc.ApplyTransition(t.Context(), monitor, tc.transition, at)
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}
			if action != ActionNone {
				t.Fatalf("action = %q, beklenen %q", action, ActionNone)
			}
		})
	}

	if got := countIncidents(t, pool, monitor.ID); got != 0 {
		t.Fatalf("incident sayısı = %d, beklenen 0", got)
	}
}

func TestStreakLengthClampsToAtLeastOne(t *testing.T) {
	cases := map[int32]int32{-3: 1, 0: 1, 1: 1, 7: 7}

	for in, want := range cases {
		if got := streakLength(in); got != want {
			t.Errorf("streakLength(%d) = %d, beklenen %d", in, got, want)
		}
	}
}

// --- yardımcılar -------------------------------------------------------------

func openIncidentFor(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID) store.Incident {
	t.Helper()

	for _, incident := range listIncidents(t, pool, monitorID) {
		if incident.ResolvedAt == nil {
			return incident
		}
	}

	t.Fatalf("monitor %v için açık incident yok", monitorID)

	return store.Incident{}
}

func listIncidents(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID) []store.Incident {
	t.Helper()

	// Okuma uçları 4.2'de geliyor; burada doğrudan tabloya bakılıyor.
	rows, err := pool.Query(t.Context(),
		`SELECT id, org_id, title, status, impact, auto_monitor_id, is_auto_created,
		        started_at, resolved_at, created_at, updated_at
		 FROM incidents WHERE auto_monitor_id = $1 ORDER BY started_at`, monitorID)
	if err != nil {
		t.Fatalf("incident'lar okunamadı: %v", err)
	}
	defer rows.Close()

	var out []store.Incident
	for rows.Next() {
		var i store.Incident
		if err := rows.Scan(&i.ID, &i.OrgID, &i.Title, &i.Status, &i.Impact, &i.AutoMonitorID,
			&i.IsAutoCreated, &i.StartedAt, &i.ResolvedAt, &i.CreatedAt, &i.UpdatedAt); err != nil {
			t.Fatalf("satır okunamadı: %v", err)
		}
		out = append(out, i)
	}

	return out
}

func countIncidents(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM incidents WHERE auto_monitor_id = $1`, monitorID).Scan(&count); err != nil {
		t.Fatalf("incident sayılamadı: %v", err)
	}

	return count
}

func linkedMonitors(t *testing.T, pool *pgxpool.Pool, incidentID uuid.UUID) []uuid.UUID {
	t.Helper()

	rows, err := pool.Query(t.Context(),
		`SELECT monitor_id FROM incident_monitors WHERE incident_id = $1`, incidentID)
	if err != nil {
		t.Fatalf("bağ satırları okunamadı: %v", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("satır okunamadı: %v", err)
		}
		out = append(out, id)
	}

	return out
}

// scheduleMaintenance, monitor için verilen aralıkta bir bakım penceresi açar.
//
// Doğrudan SQL: `internal/maintenance` bu paketi tanımıyor ve tanımamalı —
// buradaki soru yalnızca "kapı çalışıyor mu".
func scheduleMaintenance(
	t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, monitorID uuid.UUID, from, to time.Time,
) {
	t.Helper()

	var windowID uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO maintenance_windows (org_id, title, starts_at, ends_at)
		 VALUES ($1, 'Planlı bakım', $2, $3) RETURNING id`, orgID, from, to).Scan(&windowID); err != nil {
		t.Fatalf("bakım penceresi oluşturulamadı: %v", err)
	}

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO maintenance_window_monitors (window_id, monitor_id) VALUES ($1, $2)`,
		windowID, monitorID); err != nil {
		t.Fatalf("bakım penceresine servis bağlanamadı: %v", err)
	}
}

// 4.3'ÜN KAPISI: bakım penceresi içindeyken otomatik incident AÇILMIYOR.
func TestApplyTransitionSuppressedDuringMaintenance(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	at := time.Now().UTC().Add(-10 * time.Minute)
	scheduleMaintenance(t, pool, orgID, monitor.ID, at.Add(-time.Hour), at.Add(time.Hour))

	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)

	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionSuppressed {
		t.Fatalf("action = %q, beklenen %q", action, ActionSuppressed)
	}

	if got := countIncidents(t, pool, monitor.ID); got != 0 {
		t.Fatalf("incident sayısı = %d, beklenen 0", got)
	}
}

// Pencere BİTTİKTEN sonraki kesinti normal şekilde incident açıyor (yarı açık
// aralık: bitiş anı artık bakım değil).
func TestApplyTransitionOpensAfterMaintenanceWindow(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	windowEnd := time.Now().UTC().Add(-time.Hour)
	scheduleMaintenance(t, pool, orgID, monitor.ID, windowEnd.Add(-time.Hour), windowEnd)

	at := windowEnd
	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)

	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionOpened {
		t.Fatalf("action = %q, beklenen %q", action, ActionOpened)
	}
}

// BAŞKA BİR MONITOR'ÜN bakımı bu monitor'ü kapsamıyor.
func TestApplyTransitionIgnoresMaintenanceOfOtherMonitor(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)
	other := createMonitor(t, pool, orgID)

	at := time.Now().UTC().Add(-5 * time.Minute)
	scheduleMaintenance(t, pool, orgID, other.ID, at.Add(-time.Hour), at.Add(time.Hour))

	recordCheck(t, pool, monitor.ID, at, store.CheckOutcomeFailure)

	action, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), at)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionOpened {
		t.Fatalf("action = %q, beklenen %q", action, ActionOpened)
	}
}

// KAPI YALNIZCA AÇMADA: bakım başlamadan önce açılmış gerçek bir kesinti,
// bakım sırasında servis düzelirse KAPANIYOR (aksi hâlde bakım bitene kadar
// açık kalırdı).
func TestApplyTransitionResolvesDuringMaintenance(t *testing.T) {
	pool := testdb.Pool(t, "incidents")
	svc, _ := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID)

	base := time.Now().UTC().Add(-2 * time.Hour)
	recordCheck(t, pool, monitor.ID, base, store.CheckOutcomeFailure)
	if _, err := svc.ApplyTransition(t.Context(), monitor, downTransition(1), base); err != nil {
		t.Fatalf("incident açılamadı: %v", err)
	}

	// Kesinti sürerken bakım başlıyor.
	recovered := base.Add(time.Hour)
	scheduleMaintenance(t, pool, orgID, monitor.ID, base.Add(30*time.Minute), recovered.Add(time.Hour))

	recordCheck(t, pool, monitor.ID, recovered, store.CheckOutcomeSuccess)

	action, err := svc.ApplyTransition(t.Context(), monitor, upTransition(1), recovered)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if action != ActionResolved {
		t.Fatalf("action = %q, beklenen %q", action, ActionResolved)
	}
}
