package rollups

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu testler GERÇEK Postgres'e karşı koşuyor. Ölçülen şeylerin hiçbiri sahte
// bir store ile ölçülemezdi: `ON CONFLICT` ile idempotentlik, `percentile_disc`
// ile p95, partition DÜŞÜRMENİN gerçekten satırları yok etmesi ve budamanın
// "yalnızca toplulaştırılmış aralık" kuralı.

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type fixture struct {
	pool      *pgxpool.Pool
	queries   *store.Queries
	svc       *Service
	orgID     uuid.UUID
	monitorID uuid.UUID
}

// newFixture, bir organizasyon + monitor bırakır ve servisi SABİT bir saatle
// kurar (testler gerçek zamana bağlı olmasın).
func newFixture(t *testing.T, now time.Time) fixture {
	t.Helper()

	pool := testdb.Pool(t, "rollups")
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

	monitor, err := queries.CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                org.ID,
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

	svc := NewService(queries, discardLogger(), Options{})
	svc.now = func() time.Time { return now }

	return fixture{pool: pool, queries: queries, svc: svc, orgID: org.ID, monitorID: monitor.ID}
}

// ensurePartition, verilen güne ait ham veri partition'ını (yoksa) oluşturur.
//
// Migration'ın penceresi dün + 30 gün (2.3); geçmişe kontrol yazan testlerin
// partition'ı kendilerinin açması gerekiyor. DDL parametre kabul etmediği için
// sınırlar metne gömülüyor — değerler testin ürettiği zamanlardan geliyor.
func (f fixture) ensurePartition(t *testing.T, day time.Time) {
	t.Helper()

	day = day.UTC().Truncate(24 * time.Hour)
	name := "check_results_" + day.Format("20060102")

	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF check_results FOR VALUES FROM ('%s') TO ('%s')",
		name,
		day.Format("2006-01-02 15:04:05-07"),
		day.Add(24*time.Hour).Format("2006-01-02 15:04:05-07")))
	if err != nil {
		t.Fatalf("partition oluşturulamadı (%s): %v", name, err)
	}
}

// insertCheck, ham bir kontrol sonucu yazar.
func (f fixture) insertCheck(
	t *testing.T, at time.Time, outcome store.CheckOutcome, responseMs int32, trigger store.CheckTrigger,
) {
	t.Helper()

	params := store.InsertCheckResultParams{
		MonitorID:   f.monitorID,
		CheckedAt:   at,
		Outcome:     outcome,
		TriggeredBy: trigger,
	}

	if outcome == store.CheckOutcomeFailure {
		params.ErrorType = store.NullCheckErrorType{
			CheckErrorType: store.CheckErrorTypeTimeout, Valid: true,
		}
	} else {
		status := int32(200)
		params.HttpStatus = &status
	}

	if responseMs >= 0 {
		ms := responseMs
		params.ResponseTimeMs = &ms
	}

	if _, err := f.queries.InsertCheckResult(t.Context(), params); err != nil {
		t.Fatalf("kontrol yazılamadı (%s): %v", at, err)
	}
}

type hourlyRow struct {
	total, success, degraded, failure int32
	maintenance                       int32
	avg, p95                          *int32
	downtime                          int32
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

func (f fixture) hourly(t *testing.T, bucket time.Time) hourlyRow {
	t.Helper()

	var row hourlyRow
	err := f.pool.QueryRow(t.Context(), `
		SELECT total_checks, success_checks, degraded_checks, failure_checks,
		       maintenance_checks, avg_response_ms, p95_response_ms, downtime_seconds
		FROM uptime_rollup_hourly WHERE monitor_id = $1 AND bucket_start = $2`,
		f.monitorID, bucket).Scan(&row.total, &row.success, &row.degraded, &row.failure,
		&row.maintenance, &row.avg, &row.p95, &row.downtime)
	if err != nil {
		t.Fatalf("saatlik özet okunamadı (%s): %v", bucket, err)
	}

	return row
}

func (f fixture) hourlyCount(t *testing.T) int {
	t.Helper()

	var count int
	if err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM uptime_rollup_hourly WHERE monitor_id = $1", f.monitorID).
		Scan(&count); err != nil {
		t.Fatalf("özet sayısı okunamadı: %v", err)
	}

	return count
}

func (f fixture) rawCount(t *testing.T) int {
	t.Helper()

	var count int
	if err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM check_results WHERE monitor_id = $1", f.monitorID).
		Scan(&count); err != nil {
		t.Fatalf("ham satır sayısı okunamadı: %v", err)
	}

	return count
}

// Toplulaştırmanın çekirdeği: sayılar, ortalama, p95 ve kesinti süresi.
func TestRollupComputesBucket(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	bucket := now.Add(-time.Hour)
	f := newFixture(t, now.Add(30*time.Minute))

	// 10 kontrol: 7 başarılı, 1 yavaş, 2 başarısız.
	for i := range 7 {
		f.insertCheck(t, bucket.Add(time.Duration(i)*time.Minute),
			store.CheckOutcomeSuccess, int32(10+i*10), store.CheckTriggerScheduled)
	}
	f.insertCheck(t, bucket.Add(8*time.Minute), store.CheckOutcomeDegraded, 500, store.CheckTriggerScheduled)
	f.insertCheck(t, bucket.Add(9*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)
	f.insertCheck(t, bucket.Add(10*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	row := f.hourly(t, bucket)

	if row.total != 10 || row.success != 7 || row.degraded != 1 || row.failure != 2 {
		t.Fatalf("sayılar = %+v", row)
	}

	// Ortalama YALNIZCA ölçümü olanlardan: (10+20+…+70+500)/8 = 97.
	if row.avg == nil || *row.avg != 97 {
		t.Errorf("ortalama = %v, beklenen 97", row.avg)
	}

	// p95: 8 ölçüm sıralı [10..70, 500] → percentile_disc(0.95) = 500.
	if row.p95 == nil || *row.p95 != 500 {
		t.Errorf("p95 = %v, beklenen 500", row.p95)
	}

	// Kesinti: 2 başarısız × 60 sn aralık.
	if row.downtime != 120 {
		t.Errorf("kesinti = %d sn, beklenen 120", row.downtime)
	}
}

// İDEMPOTENTLİK — bu adımın şartı: aynı saat iki kez işlenirse veri bozulmaz.
func TestRollupIsIdempotent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	bucket := now.Add(-time.Hour)
	f := newFixture(t, now.Add(15*time.Minute))

	for i := range 5 {
		f.insertCheck(t, bucket.Add(time.Duration(i)*time.Minute),
			store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)
	}

	for range 3 {
		if _, err := f.svc.RunRollup(t.Context()); err != nil {
			t.Fatalf("toplulaştırma başarısız: %v", err)
		}
	}

	if count := f.hourlyCount(t); count != 1 {
		t.Fatalf("özet satırı = %d, beklenen 1 (satır katlanmamalı)", count)
	}

	row := f.hourly(t, bucket)
	if row.total != 5 || row.success != 5 {
		t.Errorf("sayılar = %+v, üç turdan sonra da 5 olmalıydı", row)
	}
}

// YENİDEN HESAPLAMA: kova işlendikten SONRA gelen satırlar da özete giriyor
// (geç yazılan kontroller, backfill). Upsert "üzerine yaz" olduğu için sayı
// TOPLANMIYOR, yeniden hesaplanıyor.
func TestRollupRecomputesBucketWithLateRows(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	bucket := now.Add(-time.Hour)
	f := newFixture(t, now.Add(15*time.Minute))

	f.insertCheck(t, bucket.Add(time.Minute), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	f.insertCheck(t, bucket.Add(2*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	row := f.hourly(t, bucket)
	if row.total != 2 || row.failure != 1 {
		t.Errorf("sayılar = %+v, beklenen toplam 2 / 1 başarısız", row)
	}
}

// MANUEL KONTROLLER ÖZETE GİRMİYOR (2.3'ün kararı): "şimdi kontrol et"
// düğmesine üst üste basmak uptime yüzdesini bozmamalı.
func TestRollupExcludesManualChecks(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	bucket := now.Add(-time.Hour)
	f := newFixture(t, now.Add(15*time.Minute))

	f.insertCheck(t, bucket.Add(time.Minute), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)
	f.insertCheck(t, bucket.Add(2*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerManual)
	f.insertCheck(t, bucket.Add(3*time.Minute), store.CheckOutcomeSuccess, 100, store.CheckTriggerManual)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	row := f.hourly(t, bucket)
	if row.total != 1 || row.failure != 0 {
		t.Errorf("sayılar = %+v, yalnızca zamanlanmış kontrol sayılmalıydı", row)
	}
}

// Kovalar SAAT SINIRINDA ayrılıyor ve günlük özet aynı ham veriden üretiliyor.
func TestRollupSeparatesBucketsAndFillsDaily(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	f := newFixture(t, now.Add(10*time.Minute))

	first := now.Add(-2 * time.Hour)
	second := now.Add(-time.Hour)

	f.insertCheck(t, first.Add(time.Minute), store.CheckOutcomeSuccess, 50, store.CheckTriggerScheduled)
	f.insertCheck(t, first.Add(59*time.Minute), store.CheckOutcomeSuccess, 70, store.CheckTriggerScheduled)
	f.insertCheck(t, second.Add(time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	if row := f.hourly(t, first); row.total != 2 || row.success != 2 {
		t.Errorf("ilk kova = %+v", row)
	}
	if row := f.hourly(t, second); row.total != 1 || row.failure != 1 {
		t.Errorf("ikinci kova = %+v", row)
	}

	// Günlük özet: aynı ham veriden (saatliklerden DEĞİL) — 3 kontrol.
	var total, failure int32
	err := f.pool.QueryRow(t.Context(), `
		SELECT total_checks, failure_checks FROM uptime_rollup_daily
		WHERE monitor_id = $1 AND bucket_date = $2::date`,
		f.monitorID, now.Format("2006-01-02")).Scan(&total, &failure)
	if err != nil {
		t.Fatalf("günlük özet okunamadı: %v", err)
	}

	if total != 3 || failure != 1 {
		t.Errorf("günlük özet = %d kontrol / %d başarısız, beklenen 3/1", total, failure)
	}
}

// DEVAM EDEN saat de özetleniyor: grafik kova dolmadan da veri gösterebilsin.
// Yarım kova bir anlık görüntü, saat kapandığında düzeliyor.
func TestRollupIncludesCurrentHour(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour).Add(20 * time.Minute)
	f := newFixture(t, now)

	f.insertCheck(t, now.Add(-5*time.Minute), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	if row := f.hourly(t, now.Truncate(time.Hour)); row.total != 1 {
		t.Errorf("devam eden saatin özeti = %+v", row)
	}
}

// GERİYE DÖNÜK DOLDURMA: worker uzun süre kapalı kaldıysa özetler su
// seviyesinden devam ediyor — "son N saat" penceresi olsaydı o günler sonsuza
// kadar boş kalırdı. Tek turda en fazla maxHourlySpan işleniyor, kalanı
// sonraki turlarda.
func TestRollupBackfillsFromOldestCheck(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	f := newFixture(t, now.Add(10*time.Minute))

	// 5 gün öncesinden bir kontrol (lookback penceresinin çok dışında).
	old := now.Add(-5 * 24 * time.Hour)
	f.ensurePartition(t, old)
	f.insertCheck(t, old.Add(time.Minute), store.CheckOutcomeSuccess, 42, store.CheckTriggerScheduled)
	f.insertCheck(t, now.Add(-30*time.Minute), store.CheckOutcomeSuccess, 42, store.CheckTriggerScheduled)

	// İlk tur: en eski kontrolden başlıyor, 48 saatlik dilimi işliyor.
	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	if row := f.hourly(t, old); row.total != 1 {
		t.Fatalf("en eski kova işlenmedi: %+v", row)
	}

	// Yetişene kadar tur atılıyor (her tur bir dilim ilerliyor).
	for range 5 {
		if _, err := f.svc.RunRollup(t.Context()); err != nil {
			t.Fatalf("toplulaştırma başarısız: %v", err)
		}
	}

	// Son kontrol yarım saat öncesine yazıldı, yani BİR ÖNCEKİ saatin kovası.
	if row := f.hourly(t, now.Add(-time.Hour)); row.total != 1 {
		t.Errorf("güncel kova işlenmedi: %+v", row)
	}
}

// Hiç ham veri yoksa job sessizce ve hatasız dönmeli (boş sistemde her 5
// dakikada bir hata loglamak, gerçek hataları gizlerdi).
func TestRollupWithoutDataIsNoop(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	stats, err := f.svc.RunRollup(t.Context())
	if err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	if stats.HourlyBuckets != 0 || stats.DailyBuckets != 0 {
		t.Errorf("boş sistemde kova üretildi: %+v", stats)
	}
}

// BUDAMANIN BİRİNCİ KAPISI: özeti üretilmemiş ham veri SİLİNMİYOR.
// (0.1 §6.3: "önce rollup, sonra prune" — aksi hâlde rollup'ın bozulduğu bir
// hafta geri dönülemez veri kaybına dönerdi.)
func TestRetentionKeepsUnaggregatedData(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	// Ham veri var ama HİÇ özet üretilmemiş.
	f.insertCheck(t, now.Add(-time.Hour), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	stats, err := f.svc.RunRetention(t.Context())
	if err != nil {
		t.Fatalf("budama başarısız: %v", err)
	}

	if stats.DroppedPartitions != 0 {
		t.Errorf("özetsiz veri budandı: %d partition", stats.DroppedPartitions)
	}
	if !stats.AggregationBlocked {
		t.Error("budamanın engellendiği bildirilmedi")
	}
	if f.rawCount(t) != 1 {
		t.Error("ham satır silindi")
	}
}

// Saklama süresi İÇİNDEKİ veri, özeti üretilmiş olsa bile silinmiyor.
func TestRetentionKeepsRecentData(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	f.insertCheck(t, now.Add(-2*time.Hour), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	if _, err := f.svc.RunRetention(t.Context()); err != nil {
		t.Fatalf("budama başarısız: %v", err)
	}

	if f.rawCount(t) != 1 {
		t.Error("saklama süresi içindeki ham satır silindi")
	}
}

// Partition penceresi İLERİ KAYDIRILIYOR: 2.3'ün fonksiyonu çağrılmazsa
// partition'lar biter ve DEFAULT partition olmadığı için yeni kontroller
// yazılamaz.
func TestRetentionExtendsPartitionWindow(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	// Pencereyi bilinçli olarak dar kur: birkaç gün ilerideki partition'lar
	// eksik kalsın.
	f.svc.opts.PartitionDaysAhead = 45

	if _, err := f.svc.RunRetention(t.Context()); err != nil {
		t.Fatalf("budama başarısız: %v", err)
	}

	// Açılışta pencere 30 gündü (migration); job onu 45 güne çıkarıyor.
	// "Kaç tane yaratıldı" DEĞİL "pencere kapsıyor mu" ölçülüyor: partition'lar
	// test veritabanında kalıcı (testdb yalnızca TRUNCATE ediyor), yani sayı
	// önceki koşulara bağlı olurdu.
	var future bool
	if err := f.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL",
		"check_results_"+now.AddDate(0, 0, 44).Format("20060102")).Scan(&future); err != nil {
		t.Fatalf("partition varlığı okunamadı: %v", err)
	}
	if !future {
		t.Error("44 gün sonrasının partition'ı yok — pencere ilerletilmedi")
	}

	var count int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_inherits i
		JOIN pg_class parent ON parent.oid = i.inhparent
		WHERE parent.relname = 'check_results'`).Scan(&count); err != nil {
		t.Fatalf("partition sayısı okunamadı: %v", err)
	}

	if count < 45 {
		t.Errorf("partition sayısı = %d, en az 45 bekleniyordu", count)
	}
}

// BUDAMA GERÇEKTEN SİLİYOR: saklama süresinden eski VE özeti üretilmiş ham
// veri, partition DÜŞÜRÜLEREK yok ediliyor (DELETE değil — 2.3'ün kararı).
func TestRetentionDropsOldAggregatedPartitions(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	// 40 gün önceki bir gün için partition + ham satır oluştur.
	old := now.AddDate(0, 0, -40).Truncate(24 * time.Hour)
	partition := "check_results_" + old.Format("20060102")

	f.ensurePartition(t, old)

	f.insertCheck(t, old.Add(time.Hour), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)
	f.insertCheck(t, now.Add(-time.Hour), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	if f.rawCount(t) != 2 {
		t.Fatalf("hazırlık: ham satır = %d", f.rawCount(t))
	}

	// Toplulaştırma 40 günü kapsayana kadar tur at (her tur bir dilim).
	for range 4 {
		if _, err := f.svc.RunRollup(t.Context()); err != nil {
			t.Fatalf("toplulaştırma başarısız: %v", err)
		}
	}

	stats, err := f.svc.RunRetention(t.Context())
	if err != nil {
		t.Fatalf("budama başarısız: %v", err)
	}

	if stats.DroppedPartitions == 0 {
		t.Fatalf("eski partition düşürülmedi (cutoff %s)", stats.RawCutoff)
	}

	// Eski satır gitti, yenisi duruyor.
	if got := f.rawCount(t); got != 1 {
		t.Errorf("ham satır = %d, beklenen 1", got)
	}

	// Ama ÖZET duruyor: geçmiş yalnızca orada yaşıyor.
	var total int32
	if err := f.pool.QueryRow(t.Context(), `
		SELECT total_checks FROM uptime_rollup_daily
		WHERE monitor_id = $1 AND bucket_date = $2::date`,
		f.monitorID, old.Format("2006-01-02")).Scan(&total); err != nil {
		t.Fatalf("eski günün özeti kayboldu: %v", err)
	}
	if total != 1 {
		t.Errorf("eski günün özeti = %d kontrol, beklenen 1", total)
	}

	// Partition tablosu gerçekten yok.
	var exists bool
	if err := f.pool.QueryRow(t.Context(),
		"SELECT to_regclass($1) IS NOT NULL", partition).Scan(&exists); err != nil {
		t.Fatalf("partition varlığı okunamadı: %v", err)
	}
	if exists {
		t.Error("partition tablosu hâlâ duruyor")
	}
}

// Saatlik özetler 13 aydan sonra siliniyor; GÜNLÜK özetler hiç silinmiyor.
func TestRetentionPrunesOldHourlyRollupsOnly(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	oldBucket := now.AddDate(0, 0, -500).Truncate(time.Hour)

	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO uptime_rollup_hourly
			(monitor_id, bucket_start, total_checks, success_checks, degraded_checks, failure_checks)
		VALUES ($1, $2, 10, 10, 0, 0)`, f.monitorID, oldBucket); err != nil {
		t.Fatalf("eski saatlik özet yazılamadı: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO uptime_rollup_daily
			(monitor_id, bucket_date, total_checks, success_checks, degraded_checks, failure_checks)
		VALUES ($1, $2::date, 10, 10, 0, 0)`,
		f.monitorID, oldBucket.Format("2006-01-02")); err != nil {
		t.Fatalf("eski günlük özet yazılamadı: %v", err)
	}

	stats, err := f.svc.RunRetention(t.Context())
	if err != nil {
		t.Fatalf("budama başarısız: %v", err)
	}

	if stats.DeletedHourlyRows != 1 {
		t.Errorf("silinen saatlik satır = %d, beklenen 1", stats.DeletedHourlyRows)
	}

	var dailyCount int
	if err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM uptime_rollup_daily WHERE monitor_id = $1", f.monitorID).
		Scan(&dailyCount); err != nil {
		t.Fatalf("günlük özet sayısı okunamadı: %v", err)
	}

	if dailyCount != 1 {
		t.Error("günlük özet silindi — süresiz saklanmalıydı")
	}
}

// Monitor silinince özetleri de gidiyor (cascade): yetim satır kalmıyor.
func TestRollupsCascadeOnMonitorDelete(t *testing.T) {
	now := time.Now().UTC()
	f := newFixture(t, now)

	f.insertCheck(t, now.Add(-30*time.Minute), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(), "DELETE FROM monitors WHERE id = $1", f.monitorID); err != nil {
		t.Fatalf("monitor silinemedi: %v", err)
	}

	if count := f.hourlyCount(t); count != 0 {
		t.Errorf("monitor silindikten sonra %d saatlik özet kaldı", count)
	}
}

// Job'lar runtime'a kaydedilebilir olmalı.
func TestJobsAreRegisterable(t *testing.T) {
	svc := NewService(nil, discardLogger(), Options{})

	rollup := svc.RollupJob(5 * time.Minute)
	retention := svc.RetentionJob(time.Hour)

	if rollup.Name != RollupJobName || retention.Name != RetentionJobName {
		t.Fatalf("job adları = %q / %q", rollup.Name, retention.Name)
	}
	if rollup.Run == nil || retention.Run == nil {
		t.Fatal("job gövdesi boş")
	}
}

// BAKIM PENCERESİ (4.3): pencereye düşen kontroller `maintenance_checks`
// olarak sayılıyor ve success/degraded/failure'a GİRMİYOR.
//
// Bölüşüm bu şekilde olmak zorunda: 0.1'in formülü ((success + degraded) /
// (total - maintenance)) aksi hâlde %100'ü aşabilirdi.
func TestRollupSeparatesMaintenanceChecks(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	bucket := now.Add(-time.Hour)
	f := newFixture(t, now.Add(30*time.Minute))

	// Kovanın ilk 10 dakikası bakımda.
	f.scheduleMaintenance(t, bucket, bucket.Add(10*time.Minute))

	// Bakım içinde: 1 başarılı (çok yavaş) + 2 başarısız.
	f.insertCheck(t, bucket.Add(time.Minute), store.CheckOutcomeSuccess, 5000, store.CheckTriggerScheduled)
	f.insertCheck(t, bucket.Add(2*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)
	f.insertCheck(t, bucket.Add(3*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)

	// Bakım dışında: 3 başarılı + 1 başarısız.
	for i := range 3 {
		f.insertCheck(t, bucket.Add(time.Duration(20+i)*time.Minute),
			store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)
	}
	f.insertCheck(t, bucket.Add(30*time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	row := f.hourly(t, bucket)

	if row.total != 7 {
		t.Errorf("toplam = %d, beklenen 7", row.total)
	}
	if row.maintenance != 3 {
		t.Errorf("bakım kontrolü = %d, beklenen 3", row.maintenance)
	}
	if row.success != 3 || row.degraded != 0 || row.failure != 1 {
		t.Errorf("sayılar = %+v, beklenen 3 başarı / 0 yavaş / 1 hata", row)
	}

	// Toplam = bakım + (başarı + yavaş + hata): dört parça, bütünü aşmıyor.
	if row.maintenance+row.success+row.degraded+row.failure != row.total {
		t.Errorf("parçalar bütünü tutmuyor: %+v", row)
	}

	// Uptime = (3 + 0) / (7 - 3) = %75 — bakımdaki başarılı kontrol oranı
	// yukarı çekmiyor, bakımdaki hatalar da aşağı.
	uptime := float64(row.success+row.degraded) / float64(row.total-row.maintenance) * 100
	if uptime != 75 {
		t.Errorf("uptime = %.2f, beklenen 75", uptime)
	}

	// Yanıt süresi istatistikleri de bakım kontrollerini dışarıda bırakıyor:
	// 5000 ms'lik bakım ölçümü ortalamayı bozmamalı.
	if row.avg == nil || *row.avg != 100 {
		t.Errorf("ortalama = %v, beklenen 100", row.avg)
	}
	if row.p95 == nil || *row.p95 != 100 {
		t.Errorf("p95 = %v, beklenen 100", row.p95)
	}

	// Kesinti süresi de yalnızca bakım DIŞI hatalardan: 1 × 60 sn.
	if row.downtime != 60 {
		t.Errorf("kesinti = %d sn, beklenen 60", row.downtime)
	}
}

// Günlük özet de aynı bölüşümü uyguluyor.
func TestDailyRollupSeparatesMaintenanceChecks(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	bucket := now.Add(-2 * time.Hour)
	f := newFixture(t, now.Add(30*time.Minute))

	f.scheduleMaintenance(t, bucket, bucket.Add(30*time.Minute))

	f.insertCheck(t, bucket.Add(time.Minute), store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)
	f.insertCheck(t, bucket.Add(45*time.Minute), store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	var total, success, failure, maintenance int32
	if err := f.pool.QueryRow(t.Context(), `
		SELECT total_checks, success_checks, failure_checks, maintenance_checks
		FROM uptime_rollup_daily WHERE monitor_id = $1 AND bucket_date = $2`,
		f.monitorID, bucket.Format("2006-01-02")).
		Scan(&total, &success, &failure, &maintenance); err != nil {
		t.Fatalf("günlük özet okunamadı: %v", err)
	}

	if total != 2 || maintenance != 1 || success != 1 || failure != 0 {
		t.Fatalf("günlük sayılar = toplam %d / bakım %d / başarı %d / hata %d",
			total, maintenance, success, failure)
	}
}

// TOPLULAŞTIRMA (3.4) İLE DETAY ÖZETİ (2.4) AYNI GÜNE AYNI CEVABI VERİYOR.
//
// İki ayrı SQL, tek bir tanım: `(success + degraded) / (total - maintenance)`,
// DEGRADED "up" sayılır, bakım paydadan düşer, manuel kontroller hiç sayılmaz.
// Mimari kararlarda yazılı olan ("ayrışsalardı aynı gün için iki farklı uptime
// cevabı doğardı") bu adıma kadar bir TEMENNİYDİ: iki sorgunun da kendi testi
// vardı ama BİRBİRİYLE karşılaştırıldıkları bir yer yoktu. Bir tarafta
// unutulan bir süzgeç — mesela günlük özetin manuel kontrolleri sayması —
// diğer tarafın testlerini yeşil bırakırdı.
func TestDailyRollupAndDetailSummaryAgree(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	// Kova DÜNDE: bugünün henüz kapanmamış saatleri iki tarafta farklı
	// pencerelere düşmesin.
	day := now.Add(-24 * time.Hour).Truncate(24 * time.Hour)
	f := newFixture(t, now)

	f.ensurePartition(t, day)

	// Bakım penceresi günün ilk saatinde; içine hem başarı hem hata düşüyor.
	f.scheduleMaintenance(t, day.Add(time.Hour), day.Add(2*time.Hour))

	// Bakım DIŞI: 5 başarı + 2 degraded + 3 hata → (5+2)/10 = %70.
	for i := range 5 {
		f.insertCheck(t, day.Add(4*time.Hour+time.Duration(i)*time.Minute),
			store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)
	}
	for i := range 2 {
		f.insertCheck(t, day.Add(5*time.Hour+time.Duration(i)*time.Minute),
			store.CheckOutcomeDegraded, 900, store.CheckTriggerScheduled)
	}
	for i := range 3 {
		f.insertCheck(t, day.Add(6*time.Hour+time.Duration(i)*time.Minute),
			store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)
	}

	// Bakım İÇİ: paydadan düşmeli ama oranı da yukarı çekmemeli (4.3).
	f.insertCheck(t, day.Add(time.Hour+10*time.Minute),
		store.CheckOutcomeSuccess, 100, store.CheckTriggerScheduled)
	f.insertCheck(t, day.Add(time.Hour+20*time.Minute),
		store.CheckOutcomeFailure, -1, store.CheckTriggerScheduled)

	// MANUEL: iki tarafın da hesabının DIŞINDA (2.3).
	f.insertCheck(t, day.Add(7*time.Hour), store.CheckOutcomeFailure, -1, store.CheckTriggerManual)

	if _, err := f.svc.RunRollup(t.Context()); err != nil {
		t.Fatalf("toplulaştırma başarısız: %v", err)
	}

	var rollup struct {
		total, success, degraded, failure, maintenance int32
	}
	if err := f.pool.QueryRow(t.Context(), `
		SELECT total_checks, success_checks, degraded_checks, failure_checks, maintenance_checks
		FROM uptime_rollup_daily WHERE monitor_id = $1 AND bucket_date = $2`,
		f.monitorID, day.Format("2006-01-02")).
		Scan(&rollup.total, &rollup.success, &rollup.degraded, &rollup.failure,
			&rollup.maintenance); err != nil {
		t.Fatalf("günlük özet okunamadı: %v", err)
	}

	summary, err := results.NewService(f.queries, discardLogger()).
		Summarize(t.Context(), f.orgID, f.monitorID, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("detay özeti okunamadı: %v", err)
	}

	// Bölüşümün DÖRT parçası da birebir aynı olmalı.
	if int64(rollup.total) != summary.ScheduledChecks {
		t.Errorf("toplam: rollup %d, özet %d", rollup.total, summary.ScheduledChecks)
	}
	if int64(rollup.success) != summary.SuccessChecks {
		t.Errorf("başarı: rollup %d, özet %d", rollup.success, summary.SuccessChecks)
	}
	if int64(rollup.degraded) != summary.DegradedChecks {
		t.Errorf("degraded: rollup %d, özet %d", rollup.degraded, summary.DegradedChecks)
	}
	if int64(rollup.failure) != summary.FailureChecks {
		t.Errorf("hata: rollup %d, özet %d", rollup.failure, summary.FailureChecks)
	}
	if int64(rollup.maintenance) != summary.MaintenanceChecks {
		t.Errorf("bakım: rollup %d, özet %d", rollup.maintenance, summary.MaintenanceChecks)
	}

	// Manuel kontrol İKİSİNE DE girmemiş olmalı — özet onu ayrıca sayıyor.
	if summary.ManualChecks != 1 {
		t.Errorf("manuel kontrol sayısı = %d, beklenen 1", summary.ManualChecks)
	}
	if rollup.total != 12 {
		t.Errorf("günlük toplam = %d, beklenen 12 (manuel kontrol sayılmamalı)", rollup.total)
	}

	// Ve aynı formül iki tarafta da aynı yüzdeyi vermeli.
	fromRollup := float64(rollup.success+rollup.degraded) /
		float64(rollup.total-rollup.maintenance) * 100
	fromSummary := summary.UptimePercent()

	if fromSummary == nil {
		t.Fatal("özet uptime null döndü")
	}
	if diff := *fromSummary - fromRollup; diff > 0.01 || diff < -0.01 {
		t.Errorf("uptime ayrışıyor: rollup %.2f, özet %.2f", fromRollup, *fromSummary)
	}
	if fromRollup < 69.99 || fromRollup > 70.01 {
		t.Errorf("uptime = %.2f, beklenen 70 (degraded UP, bakım paydadan düşer)", fromRollup)
	}
}
