package results

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu testler GERÇEK Postgres'e karşı koşuyor. Ölçülen şeylerin hiçbiri sahte
// bir store ile ölçülemezdi: partition'a yazma, şemadaki tutarlılık
// kısıtları, org izolasyonunun JOIN'e dayanması ve cascade silme.

func newTestService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()

	return NewService(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fixture, bir organizasyon ve ona ait bir monitor bırakır.
type fixture struct {
	pool      *pgxpool.Pool
	svc       *Service
	orgID     uuid.UUID
	monitorID uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	// testdb.Pool tabloları TEMİZLİYOR — bu yüzden tek testte iki kez
	// çağrılamaz (ikincisi birincinin verisini siler). İkinci bir
	// organizasyon gerektiğinde createOrg kullanılıyor.
	pool := testdb.Pool(t, "results")
	orgID := createOrg(t, pool)

	return fixture{
		pool:      pool,
		svc:       newTestService(t, pool),
		orgID:     orgID,
		monitorID: createMonitor(t, pool, orgID),
	}
}

// createOrg, sahibi olan yeni bir organizasyon oluşturur (tabloları
// temizlemeden).
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

func createMonitor(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID) uuid.UUID {
	t.Helper()

	monitor, err := store.New(pool).CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                orgID,
		Name:                 "API",
		Url:                  "https://example.com/health",
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: 60,
		TimeoutMs:            10000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		FailureThreshold:     3,
		RecoveryThreshold:    2,
		IsActive:             true,
		CurrentStatus:        store.MonitorStatusPending,
	})
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	return monitor.ID
}

func successAt(at time.Time) checker.Result {
	return checker.Result{
		Outcome:      store.CheckOutcomeSuccess,
		HTTPStatus:   200,
		ResponseTime: 120 * time.Millisecond,
		CheckedAt:    at,
	}
}

func TestRecordStoresSuccess(t *testing.T) {
	f := newFixture(t)

	row, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled,
		successAt(time.Now().UTC()))
	if err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	if row.ID == 0 {
		t.Error("id üretilmemiş")
	}
	if row.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("outcome = %q", row.Outcome)
	}
	if row.TriggeredBy != store.CheckTriggerScheduled {
		t.Errorf("kaynak = %q", row.TriggeredBy)
	}
	if row.HttpStatus == nil || *row.HttpStatus != 200 {
		t.Errorf("status = %v", row.HttpStatus)
	}
	if row.ResponseTimeMs == nil || *row.ResponseTimeMs != 120 {
		t.Errorf("süre = %v", row.ResponseTimeMs)
	}
	// Başarılı kontrolde hata alanları BOŞ (şema da bunu şart koşuyor).
	if row.ErrorType.Valid || row.ErrorMessage != nil {
		t.Errorf("hata alanları dolu: %+v", row)
	}
}

// Cevap alınamayan kontrolde status ve süre NULL kalıyor — 0 uydurulmuyor.
func TestRecordStoresFailureWithoutResponse(t *testing.T) {
	f := newFixture(t)

	row, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled, checker.Result{
		Outcome:      store.CheckOutcomeFailure,
		ErrorType:    store.CheckErrorTypeDns,
		ErrorMessage: "no such host",
		CheckedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	if row.HttpStatus != nil {
		t.Errorf("status = %v, null olmalıydı", *row.HttpStatus)
	}
	if row.ResponseTimeMs != nil {
		t.Errorf("süre = %v, null olmalıydı", *row.ResponseTimeMs)
	}
	if !row.ErrorType.Valid || row.ErrorType.CheckErrorType != store.CheckErrorTypeDns {
		t.Errorf("hata tipi = %+v", row.ErrorType)
	}
	if row.ErrorMessage == nil || *row.ErrorMessage != "no such host" {
		t.Errorf("mesaj = %v", row.ErrorMessage)
	}
}

// Hata tipi olmayan başarısız sonuç veriyi KAYBETTİRMEMELİ: `unknown` olarak
// yazılıyor (ve loglanıyor).
func TestRecordFillsMissingErrorType(t *testing.T) {
	f := newFixture(t)

	row, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled, checker.Result{
		Outcome:   store.CheckOutcomeFailure,
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	if !row.ErrorType.Valid || row.ErrorType.CheckErrorType != store.CheckErrorTypeUnknown {
		t.Errorf("hata tipi = %+v, beklenen unknown", row.ErrorType)
	}
}

// 1 ms'den kısa süren başarılı kontrol: süre SIFIR ama ölçüm var.
// (Koşul "süre > 0" olsaydı şemadaki tutarlılık kısıtı satırı reddederdi —
// yerel bir hedefe yapılan kontrolde bu gerçekten oluyor.)
func TestRecordStoresZeroDurationSuccess(t *testing.T) {
	f := newFixture(t)

	row, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerManual, checker.Result{
		Outcome:      store.CheckOutcomeSuccess,
		HTTPStatus:   200,
		ResponseTime: 400 * time.Microsecond,
		CheckedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	if row.ResponseTimeMs == nil || *row.ResponseTimeMs != 0 {
		t.Errorf("süre = %v, beklenen 0", row.ResponseTimeMs)
	}
}

func TestRecordStoresDegraded(t *testing.T) {
	f := newFixture(t)

	row, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerManual, checker.Result{
		Outcome:      store.CheckOutcomeDegraded,
		HTTPStatus:   200,
		ResponseTime: 900 * time.Millisecond,
		CheckedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	if row.Outcome != store.CheckOutcomeDegraded {
		t.Errorf("outcome = %q", row.Outcome)
	}
	if row.TriggeredBy != store.CheckTriggerManual {
		t.Errorf("kaynak = %q", row.TriggeredBy)
	}
	// DEGRADED bir hata DEĞİL (0.1: uptime hesabında "up" sayılıyor).
	if row.ErrorType.Valid {
		t.Errorf("degraded satırında hata tipi var: %+v", row.ErrorType)
	}
}

// ŞEMA SON HAKEM: tutarsız satır (başarısız ama hata tipi yok / başarılı ama
// hata dolu) doğrulama atlanarak yazılmaya çalışılırsa veritabanı reddediyor.
func TestSchemaRejectsInconsistentRows(t *testing.T) {
	f := newFixture(t)
	queries := store.New(f.pool)
	now := time.Now().UTC()

	tests := []struct {
		name   string
		params store.InsertCheckResultParams
	}{
		{
			name: "hata tipi olmayan başarısız kontrol",
			params: store.InsertCheckResultParams{
				MonitorID: f.monitorID, CheckedAt: now,
				Outcome: store.CheckOutcomeFailure, TriggeredBy: store.CheckTriggerScheduled,
			},
		},
		{
			name: "hata tipi dolu başarılı kontrol",
			params: store.InsertCheckResultParams{
				MonitorID: f.monitorID, CheckedAt: now,
				Outcome: store.CheckOutcomeSuccess, TriggeredBy: store.CheckTriggerScheduled,
				HttpStatus: ptr(int32(200)), ResponseTimeMs: ptr(int32(10)),
				ErrorType: store.NullCheckErrorType{CheckErrorType: store.CheckErrorTypeTimeout, Valid: true},
			},
		},
		{
			name: "ölçümsüz başarılı kontrol",
			params: store.InsertCheckResultParams{
				MonitorID: f.monitorID, CheckedAt: now,
				Outcome: store.CheckOutcomeSuccess, TriggeredBy: store.CheckTriggerScheduled,
			},
		},
		{
			name: "geçersiz http status",
			params: store.InsertCheckResultParams{
				MonitorID: f.monitorID, CheckedAt: now,
				Outcome: store.CheckOutcomeSuccess, TriggeredBy: store.CheckTriggerScheduled,
				HttpStatus: ptr(int32(999)), ResponseTimeMs: ptr(int32(10)),
			},
		},
		{
			name: "negatif yanıt süresi",
			params: store.InsertCheckResultParams{
				MonitorID: f.monitorID, CheckedAt: now,
				Outcome: store.CheckOutcomeSuccess, TriggeredBy: store.CheckTriggerScheduled,
				HttpStatus: ptr(int32(200)), ResponseTimeMs: ptr(int32(-1)),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := queries.InsertCheckResult(t.Context(), tt.params); err == nil {
				t.Error("veritabanı satırı reddetmeliydi")
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }

func TestListRecentReturnsNewestFirst(t *testing.T) {
	f := newFixture(t)
	// Saniyeye yuvarlanıyor: Postgres timestamptz'i MİKROSANİYE hassasiyetle
	// saklıyor, Go'nun nanosaniyeleri karşılaştırmayı boşuna bozardı.
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	for i := range 5 {
		if _, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled,
			successAt(base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("kaydedilemedi: %v", err)
		}
	}

	rows, err := f.svc.ListRecent(t.Context(), f.orgID, f.monitorID, 3)
	if err != nil {
		t.Fatalf("okunamadı: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("satır sayısı = %d, beklenen 3", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CheckedAt.After(rows[i-1].CheckedAt) {
			t.Errorf("sıralama yeniden eskiye değil: %v", rows)
		}
	}
	// En yeni kayıt başta olmalı.
	if !rows[0].CheckedAt.Equal(base.Add(4 * time.Minute)) {
		t.Errorf("ilk satır = %v", rows[0].CheckedAt)
	}
}

// ORG İZOLASYONU: başka organizasyonun monitor'ünün geçmişi okunamıyor.
// Kontrol SQL'de (JOIN + org_id), yani handler'ın doğru davranmasına
// bağlı değil.
func TestListIsScopedToOrganization(t *testing.T) {
	f := newFixture(t)
	otherOrgID := createOrg(t, f.pool)

	if _, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled,
		successAt(time.Now().UTC())); err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	// Doğru org: görüyor.
	rows, err := f.svc.ListRecent(t.Context(), f.orgID, f.monitorID, 10)
	if err != nil {
		t.Fatalf("okunamadı: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("satır sayısı = %d", len(rows))
	}

	// Yabancı org, doğru monitor kimliği: BOŞ.
	rows, err = f.svc.ListRecent(t.Context(), otherOrgID, f.monitorID, 10)
	if err != nil {
		t.Fatalf("okunamadı: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("yabancı organizasyon %d satır gördü", len(rows))
	}

	from := time.Now().UTC().Add(-24 * time.Hour)
	rows, err = f.svc.ListInRange(t.Context(), otherOrgID, f.monitorID, from, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("okunamadı: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("aralık sorgusunda yabancı organizasyon %d satır gördü", len(rows))
	}
}

// Aralık YARI AÇIK: [from, to) — ardışık pencereler aynı satırı iki kez
// saymasın (3.4'ün toplulaştırması buna dayanacak).
func TestListInRangeIsHalfOpen(t *testing.T) {
	f := newFixture(t)

	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for _, at := range []time.Time{start, start.Add(30 * time.Minute), start.Add(time.Hour)} {
		if _, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled,
			successAt(at)); err != nil {
			t.Fatalf("kaydedilemedi: %v", err)
		}
	}

	rows, err := f.svc.ListInRange(t.Context(), f.orgID, f.monitorID, start, start.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("okunamadı: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("satır sayısı = %d, beklenen 2 (üst sınır hariç)", len(rows))
	}
}

// Kontroller GÜN SINIRINI aşarak birden fazla partition'a yazılıyor ve tek
// sorguda geri okunabiliyor — partition'lama sorgu sonucunu değiştirmiyor.
func TestRowsSpanMultiplePartitions(t *testing.T) {
	f := newFixture(t)

	today := time.Now().UTC()
	yesterday := today.Add(-24 * time.Hour)

	for _, at := range []time.Time{yesterday, today} {
		if _, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled,
			successAt(at)); err != nil {
			t.Fatalf("kaydedilemedi: %v", err)
		}
	}

	// Satırlar gerçekten AYRI partition'larda mı?
	var partitions int
	row := f.pool.QueryRow(t.Context(),
		`SELECT count(DISTINCT tableoid) FROM check_results WHERE monitor_id = $1`, f.monitorID)
	if err := row.Scan(&partitions); err != nil {
		t.Fatalf("partition sayısı okunamadı: %v", err)
	}
	if partitions != 2 {
		t.Errorf("partition sayısı = %d, beklenen 2", partitions)
	}

	rows, err := f.svc.ListRecent(t.Context(), f.orgID, f.monitorID, 10)
	if err != nil {
		t.Fatalf("okunamadı: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("satır sayısı = %d", len(rows))
	}
}

// Aralık sorgusu PARTITION PRUNING'den faydalanmalı: bir günlük pencere
// otuz günün partition'larını taramamalı.
func TestRangeQueryPrunesPartitions(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Truncate(24 * time.Hour)
	to := from.Add(24 * time.Hour)

	// EXPLAIN her satırı ayrı bir kayıt olarak döndürüyor.
	rows, err := f.pool.Query(t.Context(), `
		EXPLAIN (FORMAT TEXT)
		SELECT c.id FROM check_results c
		JOIN monitors m ON m.id = c.monitor_id
		WHERE c.monitor_id = $1 AND m.org_id = $2
		  AND c.checked_at >= $3 AND c.checked_at < $4`,
		f.monitorID, f.orgID, from, to)
	if err != nil {
		t.Fatalf("plan alınamadı: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("plan satırı okunamadı: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan okunamadı: %v", err)
	}

	// Plan YALNIZCA o günün partition'ına inmeli: partition pruning'in
	// gerçekten çalıştığının kanıtı (diğer 90 partition planda yok).
	expected := "check_results_" + from.Format("20060102")
	if !strings.Contains(plan.String(), expected) {
		t.Errorf("plan beklenen partition'ı içermiyor (%s):\n%s", expected, plan.String())
	}

	// "on <tablo>" plandaki TARANAN ilişkiyi gösteriyor; indeks adları da
	// partition adını taşıdığı için düz sayım yanıltıcı olurdu.
	if scanned := strings.Count(plan.String(), "on check_results_"); scanned != 1 {
		t.Errorf("planda %d partition taranıyor, beklenen 1:\n%s", scanned, plan.String())
	}
}

// Monitor silinince geçmişi de gidiyor (ON DELETE CASCADE): monitor'süz ham
// veri anlamsız (0.1 §7).
func TestDeletingMonitorCascadesToResults(t *testing.T) {
	f := newFixture(t)

	if _, err := f.svc.Record(t.Context(), f.monitorID, store.CheckTriggerScheduled,
		successAt(time.Now().UTC())); err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(), `DELETE FROM monitors WHERE id = $1`, f.monitorID); err != nil {
		t.Fatalf("monitor silinemedi: %v", err)
	}

	var count int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM check_results WHERE monitor_id = $1`, f.monitorID).Scan(&count); err != nil {
		t.Fatalf("sayım okunamadı: %v", err)
	}
	if count != 0 {
		t.Errorf("kontrol satırı kalmış: %d", count)
	}
}

// --- Özet (2.4) ---------------------------------------------------------------

// recordAt, özet testleri için tek bir sonuç yazar.
func (f fixture) recordAt(
	t *testing.T, at time.Time, trigger store.CheckTrigger, result checker.Result,
) {
	t.Helper()

	result.CheckedAt = at
	if _, err := f.svc.Record(t.Context(), f.monitorID, trigger, result); err != nil {
		t.Fatalf("kaydedilemedi: %v", err)
	}
}

func failureResult() checker.Result {
	return checker.Result{
		Outcome:      store.CheckOutcomeFailure,
		ErrorType:    store.CheckErrorTypeTimeout,
		ErrorMessage: "hedef zamanında cevap vermedi",
	}
}

func degradedResult() checker.Result {
	return checker.Result{
		Outcome:      store.CheckOutcomeDegraded,
		HTTPStatus:   200,
		ResponseTime: 900 * time.Millisecond,
	}
}

// 0.1'in BAĞLAYICI formülü: (success + degraded) / total. DEGRADED "up"
// sayılıyor.
func TestSummarizeCountsDegradedAsUp(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	// 7 başarılı + 2 yavaş + 1 başarısız = %90 uptime.
	for i := range 7 {
		f.recordAt(t, from.Add(time.Duration(i)*time.Minute), store.CheckTriggerScheduled, successAt(time.Time{}))
	}
	f.recordAt(t, from.Add(8*time.Minute), store.CheckTriggerScheduled, degradedResult())
	f.recordAt(t, from.Add(9*time.Minute), store.CheckTriggerScheduled, degradedResult())
	f.recordAt(t, from.Add(10*time.Minute), store.CheckTriggerScheduled, failureResult())

	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}

	if summary.ScheduledChecks != 10 {
		t.Errorf("toplam = %d", summary.ScheduledChecks)
	}
	if summary.SuccessChecks != 7 || summary.DegradedChecks != 2 || summary.FailureChecks != 1 {
		t.Errorf("sayımlar = %+v", summary)
	}

	uptime := summary.UptimePercent()
	if uptime == nil {
		t.Fatal("uptime nil")
	}
	if *uptime != 90 {
		t.Errorf("uptime = %v, beklenen 90", *uptime)
	}
}

// HİÇ KONTROL YOKSA uptime nil — %100 DEĞİL.
func TestSummarizeWithoutChecksHasNilUptime(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Add(-time.Hour)
	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}

	if summary.UptimePercent() != nil {
		t.Errorf("uptime = %v, nil olmalıydı", *summary.UptimePercent())
	}
	if summary.AvgResponseMs() != nil {
		t.Errorf("ortalama = %v, nil olmalıydı", *summary.AvgResponseMs())
	}
}

// MANUEL kontroller uptime'a GİRMİYOR (2.3'ün kararı), ama sayıları
// görünüyor.
func TestSummarizeExcludesManualChecks(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	f.recordAt(t, from.Add(time.Minute), store.CheckTriggerScheduled, successAt(time.Time{}))
	// Kullanıcı düğmeye üç kez basmış ve üçü de başarısız olmuş: uptime
	// yine %100 kalmalı.
	for i := range 3 {
		f.recordAt(t, from.Add(time.Duration(10+i)*time.Minute), store.CheckTriggerManual, failureResult())
	}

	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}

	if summary.ScheduledChecks != 1 || summary.ManualChecks != 3 {
		t.Errorf("sayımlar = %+v", summary)
	}
	if uptime := summary.UptimePercent(); uptime == nil || *uptime != 100 {
		t.Errorf("uptime = %v, beklenen 100", uptime)
	}
}

// Ortalamaya yalnızca ÖLÇÜMÜ OLAN kontroller giriyor: cevap alınamayan
// kontrolü 0 ms saymak, kesintide ortalamayı düşürürdü.
func TestSummarizeAveragesOnlyMeasuredChecks(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	fast := successAt(time.Time{})
	fast.ResponseTime = 100 * time.Millisecond
	slow := successAt(time.Time{})
	slow.ResponseTime = 300 * time.Millisecond

	f.recordAt(t, from.Add(time.Minute), store.CheckTriggerScheduled, fast)
	f.recordAt(t, from.Add(2*time.Minute), store.CheckTriggerScheduled, slow)
	// Ölçümsüz başarısızlık (bağlantı kurulamadı).
	f.recordAt(t, from.Add(3*time.Minute), store.CheckTriggerScheduled, failureResult())

	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}

	average := summary.AvgResponseMs()
	if average == nil {
		t.Fatal("ortalama nil")
	}
	if *average != 200 {
		t.Errorf("ortalama = %v, beklenen 200 (ölçümsüz satır sayılmamalı)", *average)
	}
}

// Aralık dışındaki kontroller özete girmiyor ve org izolasyonu burada da
// geçerli.
func TestSummarizeRespectsRangeAndOrg(t *testing.T) {
	f := newFixture(t)
	otherOrgID := createOrg(t, f.pool)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	f.recordAt(t, from.Add(time.Minute), store.CheckTriggerScheduled, successAt(time.Time{}))
	// Aralığın dışında (üst sınır hariç).
	f.recordAt(t, to, store.CheckTriggerScheduled, failureResult())
	f.recordAt(t, from.Add(-time.Minute), store.CheckTriggerScheduled, failureResult())

	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}
	if summary.ScheduledChecks != 1 {
		t.Errorf("aralık dışı kontroller sayılmış: %+v", summary)
	}

	foreign, err := f.svc.Summarize(t.Context(), otherOrgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}
	if foreign.ScheduledChecks != 0 || foreign.UptimePercent() != nil {
		t.Errorf("yabancı organizasyon veri gördü: %+v", foreign)
	}
}

func TestLimitIsClamped(t *testing.T) {
	if got := clampLimit(0); got != DefaultListLimit {
		t.Errorf("varsayılan sınır = %d", got)
	}
	if got := clampLimit(-5); got != DefaultListLimit {
		t.Errorf("negatif sınır = %d", got)
	}
	if got := clampLimit(10_000); got != MaxListLimit {
		t.Errorf("üst sınır = %d", got)
	}
	if got := clampLimit(25); got != 25 {
		t.Errorf("geçerli sınır değişti: %d", got)
	}
}

// scheduleMaintenance, monitor için bir bakım penceresi açar (4.3).
func (f fixture) scheduleMaintenance(t *testing.T, from, to time.Time) {
	t.Helper()

	var windowID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`INSERT INTO maintenance_windows (org_id, title, starts_at, ends_at)
		 VALUES ($1, 'Planlı bakım', $2, $3) RETURNING id`,
		f.orgID, from, to).Scan(&windowID); err != nil {
		t.Fatalf("bakım penceresi oluşturulamadı: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO maintenance_window_monitors (window_id, monitor_id) VALUES ($1, $2)`,
		windowID, f.monitorID); err != nil {
		t.Fatalf("bakım penceresine servis bağlanamadı: %v", err)
	}
}

// BAKIM PENCERESİ UPTIME'IN PAYDASINDAN DÜŞÜYOR (0.1'in bağlayıcı kararı) ve
// ayrıca sayılıyor. Bakımdaki kontroller paya da girmiyor: aksi hâlde formül
// %100'ü aşabilirdi.
func TestSummarizeExcludesMaintenanceChecks(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	// İlk 10 dakika bakımda: 2 başarısız + 1 başarılı.
	f.scheduleMaintenance(t, from, from.Add(10*time.Minute))
	f.recordAt(t, from.Add(time.Minute), store.CheckTriggerScheduled, failureResult())
	f.recordAt(t, from.Add(2*time.Minute), store.CheckTriggerScheduled, failureResult())
	f.recordAt(t, from.Add(3*time.Minute), store.CheckTriggerScheduled, successAt(time.Time{}))

	// Bakım dışında: 3 başarılı + 1 başarısız → %75.
	for i := range 3 {
		f.recordAt(t, from.Add(time.Duration(20+i)*time.Minute), store.CheckTriggerScheduled,
			successAt(time.Time{}))
	}
	f.recordAt(t, from.Add(30*time.Minute), store.CheckTriggerScheduled, failureResult())

	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}

	if summary.ScheduledChecks != 7 {
		t.Errorf("zamanlanmış kontrol = %d, beklenen 7", summary.ScheduledChecks)
	}
	if summary.MaintenanceChecks != 3 {
		t.Errorf("bakım kontrolü = %d, beklenen 3", summary.MaintenanceChecks)
	}
	if summary.SuccessChecks != 3 || summary.FailureChecks != 1 {
		t.Errorf("sayımlar = %+v", summary)
	}

	uptime := summary.UptimePercent()
	if uptime == nil {
		t.Fatal("uptime nil")
	}
	if *uptime != 75 {
		t.Errorf("uptime = %v, beklenen 75", *uptime)
	}
}

// ARALIĞIN TAMAMI BAKIMDAYSA uptime nil: ölçülmüş bir uptime yok — %100 demek
// veri uydurmak olurdu ("hiç kontrol yok" durumuyla aynı gerekçe).
func TestSummarizeWithOnlyMaintenanceHasNilUptime(t *testing.T) {
	f := newFixture(t)

	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	to := from.Add(time.Hour)

	f.scheduleMaintenance(t, from, to)
	f.recordAt(t, from.Add(time.Minute), store.CheckTriggerScheduled, successAt(time.Time{}))
	f.recordAt(t, from.Add(2*time.Minute), store.CheckTriggerScheduled, failureResult())

	summary, err := f.svc.Summarize(t.Context(), f.orgID, f.monitorID, from, to)
	if err != nil {
		t.Fatalf("özet okunamadı: %v", err)
	}

	if summary.MaintenanceChecks != 2 {
		t.Errorf("bakım kontrolü = %d, beklenen 2", summary.MaintenanceChecks)
	}
	if summary.UptimePercent() != nil {
		t.Errorf("uptime = %v, nil olmalıydı", *summary.UptimePercent())
	}
}
