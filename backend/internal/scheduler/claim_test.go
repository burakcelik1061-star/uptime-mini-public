package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu dosyadaki testler GERÇEK Postgres'e karşı koşuyor ve sahte bir store ile
// ÖLÇÜLEMEYECEK şeyleri ölçüyor: `FOR UPDATE SKIP LOCKED` ile sahiplenmenin
// atomikliği, eşzamanlı iki claim'in aynı monitor'ü almaması, şemadaki
// zamanlama tutarlılık kısıtı ve uçtan uca akış (gerçek HTTP kontrolü → gerçek
// check_results satırı).

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return testdb.Pool(t, "scheduler")
}

// createOrg, sahibi olan bir organizasyon bırakır.
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

type monitorOption func(*store.CreateMonitorParams)

func withURL(url string) monitorOption {
	return func(p *store.CreateMonitorParams) { p.Url = url }
}

func withInterval(seconds int32) monitorOption {
	return func(p *store.CreateMonitorParams) { p.CheckIntervalSeconds = seconds }
}

func withTimeout(ms int32) monitorOption {
	return func(p *store.CreateMonitorParams) { p.TimeoutMs = ms }
}

func withThresholds(failure, recovery int32) monitorOption {
	return func(p *store.CreateMonitorParams) {
		p.FailureThreshold = failure
		p.RecoveryThreshold = recovery
	}
}

func inactive() monitorOption {
	return func(p *store.CreateMonitorParams) {
		p.IsActive = false
		p.CurrentStatus = store.MonitorStatusPaused
	}
}

func createMonitor(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, opts ...monitorOption) store.Monitor {
	t.Helper()

	params := store.CreateMonitorParams{
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
	}

	for _, opt := range opts {
		opt(&params)
	}

	monitor, err := store.New(pool).CreateMonitor(t.Context(), params)
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	return monitor
}

// setNextCheckAt, testin monitor'ü "sırası gelmemiş" hâle getirmesini sağlar.
func setNextCheckAt(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID, at time.Time) {
	t.Helper()

	if _, err := pool.Exec(t.Context(),
		"UPDATE monitors SET next_check_at = $2 WHERE id = $1", monitorID, at); err != nil {
		t.Fatalf("next_check_at yazılamadı: %v", err)
	}
}

func getMonitor(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID) store.Monitor {
	t.Helper()

	var monitor store.Monitor
	row := pool.QueryRow(t.Context(), `
		SELECT id, is_active, check_interval_seconds, next_check_at, last_checked_at, updated_at,
		       current_status, consecutive_failures, consecutive_successes,
		       failure_threshold, recovery_threshold, status_changed_at
		FROM monitors WHERE id = $1`, monitorID)
	if err := row.Scan(&monitor.ID, &monitor.IsActive, &monitor.CheckIntervalSeconds,
		&monitor.NextCheckAt, &monitor.LastCheckedAt, &monitor.UpdatedAt, &monitor.CurrentStatus,
		&monitor.ConsecutiveFailures, &monitor.ConsecutiveSuccesses,
		&monitor.FailureThreshold, &monitor.RecoveryThreshold, &monitor.StatusChangedAt); err != nil {
		t.Fatalf("monitor okunamadı: %v", err)
	}

	return monitor
}

// Yeni AKTİF monitor hemen sıraya giriyor (kullanıcı ilk sonucu bir kontrol
// aralığı beklemesin); PASİF monitor'ün sırası YOK.
func TestCreateMonitorSchedulesActiveOnly(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)

	active := createMonitor(t, pool, orgID)
	if active.NextCheckAt == nil {
		t.Error("aktif monitor sıraya girmedi (next_check_at NULL)")
	} else if time.Since(*active.NextCheckAt) > time.Minute {
		t.Errorf("aktif monitor'ün sırası çok ileride: %s", active.NextCheckAt)
	}

	paused := createMonitor(t, pool, orgID, inactive())
	if paused.NextCheckAt != nil {
		t.Errorf("duraklatılmış monitor sıraya girdi: %s", paused.NextCheckAt)
	}
}

// Sahiplenme sırayı İLERİ ALIYOR (aralık kadar) ama `updated_at`e DOKUNMUYOR:
// o sütun "yapılandırma değişti" demek.
func TestClaimAdvancesScheduleWithoutTouchingUpdatedAt(t *testing.T) {
	pool := newTestPool(t)
	queries := store.New(pool)
	monitor := createMonitor(t, pool, createOrg(t, pool), withInterval(300))

	claimed, err := queries.ClaimDueMonitors(t.Context(), 10)
	if err != nil {
		t.Fatalf("sahiplenme başarısız: %v", err)
	}

	if len(claimed) != 1 || claimed[0].ID != monitor.ID {
		t.Fatalf("sahiplenilen = %d satır", len(claimed))
	}

	after := getMonitor(t, pool, monitor.ID)
	if after.NextCheckAt == nil {
		t.Fatal("sıra NULL'a düştü")
	}

	// now() + 300 sn (kontrol başlangıçları arası en az aralık kadar).
	delta := time.Until(*after.NextCheckAt)
	if delta < 295*time.Second || delta > 305*time.Second {
		t.Errorf("sıradaki kontrol %s sonra, beklenen ~300s", delta)
	}

	if !after.UpdatedAt.Equal(monitor.UpdatedAt) {
		t.Errorf("updated_at değişti: %s → %s", monitor.UpdatedAt, after.UpdatedAt)
	}

	// İkinci sahiplenme boş dönmeli: satır artık sırası gelmiş değil.
	second, err := queries.ClaimDueMonitors(t.Context(), 10)
	if err != nil {
		t.Fatalf("ikinci sahiplenme başarısız: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("aynı monitor ikinci kez sahiplenildi (%d satır)", len(second))
	}
}

// Duraklatılmış ve sırası gelmemiş monitor'ler sahiplenilmiyor.
func TestClaimSkipsPausedAndFutureMonitors(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	due := createMonitor(t, pool, orgID)
	future := createMonitor(t, pool, orgID)
	setNextCheckAt(t, pool, future.ID, time.Now().Add(time.Hour))
	createMonitor(t, pool, orgID, inactive())

	claimed, err := queries.ClaimDueMonitors(t.Context(), 10)
	if err != nil {
		t.Fatalf("sahiplenme başarısız: %v", err)
	}

	if len(claimed) != 1 || claimed[0].ID != due.ID {
		ids := make([]string, 0, len(claimed))
		for _, m := range claimed {
			ids = append(ids, m.ID.String())
		}

		t.Fatalf("sahiplenilenler = %s, beklenen yalnızca %s", strings.Join(ids, ", "), due.ID)
	}
}

// BU ADIMIN EN ÖNEMLİ TESTİ: eşzamanlı sahiplenmeler AYNI monitor'ü almıyor.
//
// Sahte bir store ile ölçülemez — ölçülen şey `FOR UPDATE SKIP LOCKED`in
// davranışı ve tek ifadede kilitle+ilerlet deseninin doğruluğu. Bu, birden
// fazla worker kopyası çalıştığında (3.1'in "ölçeklendirme" gerekçesi) aynı
// monitor'ün iki kez kontrol edilmemesinin garantisi.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	const monitorCount = 12
	for range monitorCount {
		createMonitor(t, pool, orgID)
	}

	const claimers = 4

	var (
		mu    sync.Mutex
		seen  = make(map[uuid.UUID]int)
		wg    sync.WaitGroup
		total int
	)

	wg.Add(claimers)
	for range claimers {
		go func() {
			defer wg.Done()

			// Her sahiplenici HEPSİNİ istemeye çalışıyor: örtüşme varsa
			// kaçınılmaz olarak görünür.
			claimed, err := queries.ClaimDueMonitors(context.Background(), monitorCount)
			if err != nil {
				t.Errorf("sahiplenme başarısız: %v", err)

				return
			}

			mu.Lock()
			defer mu.Unlock()

			total += len(claimed)
			for _, monitor := range claimed {
				seen[monitor.ID]++
			}
		}()
	}

	wg.Wait()

	for id, count := range seen {
		if count > 1 {
			t.Errorf("monitor %s %d kez sahiplenildi", id, count)
		}
	}

	if total != monitorCount || len(seen) != monitorCount {
		t.Errorf("toplam sahiplenme = %d (%d ayrı monitor), beklenen %d",
			total, len(seen), monitorCount)
	}

	// Hepsinin sırası ilerlemiş olmalı: ikinci tur boş dönüyor.
	remaining, err := queries.ClaimDueMonitors(t.Context(), monitorCount)
	if err != nil {
		t.Fatalf("sahiplenme başarısız: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("sahiplenmeden sonra %d monitor hâlâ sırada", len(remaining))
	}
}

// Sahiplenilip ÇALIŞTIRILAMAYAN kontrolün sırası geri veriliyor; duraklatılmış
// monitor'e ise DOKUNULMUYOR (şemadaki tutarlılık kısıtı bozulmasın).
func TestReleaseMonitorClaim(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	monitor := createMonitor(t, pool, orgID)
	if _, err := queries.ClaimDueMonitors(t.Context(), 10); err != nil {
		t.Fatalf("sahiplenme başarısız: %v", err)
	}

	if err := queries.ReleaseMonitorClaim(t.Context(), monitor.ID); err != nil {
		t.Fatalf("sıra geri verilemedi: %v", err)
	}

	released, err := queries.ClaimDueMonitors(t.Context(), 10)
	if err != nil {
		t.Fatalf("sahiplenme başarısız: %v", err)
	}
	if len(released) != 1 {
		t.Fatalf("sıra geri verildikten sonra sahiplenilen = %d, beklenen 1", len(released))
	}

	paused := createMonitor(t, pool, orgID, inactive())
	if err := queries.ReleaseMonitorClaim(t.Context(), paused.ID); err != nil {
		t.Fatalf("duraklatılmış monitor'de hata: %v", err)
	}

	if after := getMonitor(t, pool, paused.ID); after.NextCheckAt != nil {
		t.Errorf("duraklatılmış monitor sıraya alındı: %s", after.NextCheckAt)
	}
}

func TestApplyCheckOutcomeWritesLedger(t *testing.T) {
	pool := newTestPool(t)
	queries := store.New(pool)
	monitor := createMonitor(t, pool, createOrg(t, pool))

	checkedAt := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Millisecond)
	if err := queries.ApplyCheckOutcome(t.Context(), store.ApplyCheckOutcomeParams{
		ID:                   monitor.ID,
		CheckedAt:            &checkedAt,
		Status:               store.MonitorStatusUp,
		ConsecutiveFailures:  0,
		ConsecutiveSuccesses: 1,
	}); err != nil {
		t.Fatalf("defter yazılamadı: %v", err)
	}

	after := getMonitor(t, pool, monitor.ID)
	if after.LastCheckedAt == nil || !after.LastCheckedAt.Equal(checkedAt) {
		t.Errorf("last_checked_at = %v, beklenen %s", after.LastCheckedAt, checkedAt)
	}
	if after.CurrentStatus != store.MonitorStatusUp {
		t.Errorf("current_status = %q, beklenen up", after.CurrentStatus)
	}
	if !after.StatusChangedAt.Equal(checkedAt) {
		t.Errorf("status_changed_at = %s, beklenen %s (durum değişti)", after.StatusChangedAt, checkedAt)
	}

	// DURUM DEĞİŞMEDİYSE `status_changed_at` KORUNUYOR: her kontrolde
	// tazelenseydi "ne zamandır up?" sorusunun cevabı hep "az önce" olurdu.
	later := checkedAt.Add(30 * time.Second)
	if err := queries.ApplyCheckOutcome(t.Context(), store.ApplyCheckOutcomeParams{
		ID:                   monitor.ID,
		CheckedAt:            &later,
		Status:               store.MonitorStatusUp,
		ConsecutiveSuccesses: 2,
	}); err != nil {
		t.Fatalf("defter yazılamadı: %v", err)
	}

	after = getMonitor(t, pool, monitor.ID)
	if !after.StatusChangedAt.Equal(checkedAt) {
		t.Errorf("status_changed_at = %s, değişmemiş olmalıydı (%s)", after.StatusChangedAt, checkedAt)
	}
	if after.ConsecutiveSuccesses != 2 {
		t.Errorf("ardışık başarı = %d, beklenen 2", after.ConsecutiveSuccesses)
	}
}

// YARIŞ: kontrol sürerken kullanıcı monitor'ü duraklattıysa, biten kontrol
// 'paused' durumunun ÜZERİNE yazmamalı — kullanıcının az önce verdiği kararı
// ezmek olurdu.
func TestApplyCheckOutcomeSkipsPausedMonitor(t *testing.T) {
	pool := newTestPool(t)
	queries := store.New(pool)
	monitor := createMonitor(t, pool, createOrg(t, pool), inactive())

	checkedAt := time.Now().UTC()
	if err := queries.ApplyCheckOutcome(t.Context(), store.ApplyCheckOutcomeParams{
		ID:                  monitor.ID,
		CheckedAt:           &checkedAt,
		Status:              store.MonitorStatusUp,
		ConsecutiveFailures: 0,
	}); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	after := getMonitor(t, pool, monitor.ID)
	if after.CurrentStatus != store.MonitorStatusPaused {
		t.Errorf("current_status = %q, duraklatılmış kalmalıydı", after.CurrentStatus)
	}
	if after.LastCheckedAt != nil {
		t.Errorf("duraklatılmış monitor'ün defteri yazıldı: %s", after.LastCheckedAt)
	}
}

// ŞEMA SON HAKEM: "aktif ama sırası olmayan" bir monitor (sessizce hiç kontrol
// edilmeyecek satır) veya "duraklatılmış ama sırası olan" bir monitor
// veritabanına giremiyor — uygulamada bir hata olsa bile.
func TestScheduleConsistencyIsEnforcedBySchema(t *testing.T) {
	pool := newTestPool(t)
	monitor := createMonitor(t, pool, createOrg(t, pool))

	cases := map[string]string{
		"aktif ama sırasız":        "UPDATE monitors SET next_check_at = NULL WHERE id = $1",
		"duraklatılmış ama sıralı": "UPDATE monitors SET is_active = false WHERE id = $1",
	}

	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), query, monitor.ID); err == nil {
				t.Fatal("tutarsız zamanlama kabul edildi")
			} else if !strings.Contains(err.Error(), "monitors_schedule_consistency") {
				t.Errorf("hata = %v, tutarlılık kısıtı bekleniyordu", err)
			}
		})
	}
}

// UÇTAN UCA, GERÇEK HTTP + GERÇEK VERİTABANI: sahiplen → kontrol et → satırı
// yaz → defteri güncelle.
func TestRunDueEndToEnd(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	healthy := createMonitor(t, pool, orgID, withURL(server.URL+"/health"), withInterval(30), withTimeout(5000))
	broken := createMonitor(t, pool, orgID, withURL(failing.URL+"/health"), withInterval(30), withTimeout(5000))

	svc := NewService(queries, checker.New(safedial.Policy{AllowPrivate: true}), results.NewService(queries, discardLogger()),
		incidents.NewService(pool, queries, discardLogger()), &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 4})

	stats, err := svc.RunDue(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Claimed != 2 || stats.Recorded != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	// Sağlıklı monitor: success + ZAMANLANMIŞ tetikleyici.
	rows := listChecks(t, pool, healthy.ID)
	if len(rows) != 1 {
		t.Fatalf("kontrol satırı = %d, beklenen 1", len(rows))
	}
	if rows[0].Outcome != store.CheckOutcomeSuccess {
		t.Errorf("sonuç = %q, beklenen success", rows[0].Outcome)
	}
	if rows[0].TriggeredBy != store.CheckTriggerScheduled {
		t.Errorf("tetikleyici = %q, beklenen scheduled", rows[0].TriggeredBy)
	}
	if rows[0].ResponseTimeMs == nil {
		t.Error("yanıt süresi yazılmadı")
	}

	// Bozuk monitor: beklenmeyen status → failure + hata tipi.
	brokenRows := listChecks(t, pool, broken.ID)
	if len(brokenRows) != 1 {
		t.Fatalf("bozuk monitor kontrol satırı = %d, beklenen 1", len(brokenRows))
	}
	if brokenRows[0].Outcome != store.CheckOutcomeFailure {
		t.Errorf("sonuç = %q, beklenen failure", brokenRows[0].Outcome)
	}
	if !brokenRows[0].ErrorType.Valid ||
		brokenRows[0].ErrorType.CheckErrorType != store.CheckErrorTypeUnexpectedStatus {
		t.Errorf("hata tipi = %+v, beklenen unexpected_status", brokenRows[0].ErrorType)
	}

	// Defter: son kontrol zamanı yazıldı, sıra 30 saniye ileri alındı.
	after := getMonitor(t, pool, healthy.ID)
	if after.LastCheckedAt == nil {
		t.Error("last_checked_at yazılmadı")
	}
	if after.NextCheckAt == nil {
		t.Fatal("next_check_at NULL")
	}
	if delta := time.Until(*after.NextCheckAt); delta < 25*time.Second || delta > 35*time.Second {
		t.Errorf("sıradaki kontrol %s sonra, beklenen ~30s", delta)
	}

	// İkinci tur: sırası gelen yok, yeni satır da yok.
	second, err := svc.RunDue(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if second.Claimed != 0 {
		t.Errorf("ikinci turda %d monitor sahiplenildi, beklenen 0", second.Claimed)
	}
}

// KAPANMA SENARYOSU, GERÇEK VERİTABANINA KARŞI: kontrol yarıda kesilirse
// (worker kapanıyor) sahte bir kesinti satırı YAZILMIYOR ve sıra geri
// veriliyor — yani monitor bir tur atlamıyor.
func TestRunDueReleasesClaimOnShutdown(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	monitor := createMonitor(t, pool, orgID,
		withURL(slow.URL+"/health"), withInterval(300), withTimeout(10000))

	svc := NewService(queries, checker.New(safedial.Policy{AllowPrivate: true}), results.NewService(queries, discardLogger()),
		incidents.NewService(pool, queries, discardLogger()), &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 2})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	stats, err := svc.RunDue(ctx)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.NotRun != 1 || stats.Recorded != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	if rows := listChecks(t, pool, monitor.ID); len(rows) != 0 {
		t.Errorf("ölçüm alınmamışken %d satır yazıldı (sahte kesinti)", len(rows))
	}

	// Sıra geri verildi: monitor yeniden sahiplenilebilir olmalı.
	after := getMonitor(t, pool, monitor.ID)
	if after.NextCheckAt == nil {
		t.Fatal("next_check_at NULL")
	}
	if time.Until(*after.NextCheckAt) > time.Second {
		t.Errorf("sıra geri verilmedi: sıradaki kontrol %s", after.NextCheckAt)
	}
}

func listChecks(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID) []store.CheckResult {
	t.Helper()

	rows, err := store.New(pool).ListRecentChecks(t.Context(), store.ListRecentChecksParams{
		MonitorID: monitorID,
		OrgID:     orgOf(t, pool, monitorID),
		RowLimit:  10,
	})
	if err != nil {
		t.Fatalf("kontroller okunamadı: %v", err)
	}

	return rows
}

func orgOf(t *testing.T, pool *pgxpool.Pool, monitorID uuid.UUID) uuid.UUID {
	t.Helper()

	var orgID uuid.UUID
	if err := pool.QueryRow(t.Context(),
		"SELECT org_id FROM monitors WHERE id = $1", monitorID).Scan(&orgID); err != nil {
		t.Fatalf("org okunamadı: %v", err)
	}

	return orgID
}

// UÇTAN UCA KESİNTİ SENARYOSU, GERÇEK VERİTABANI + GERÇEK HTTP: hedef bozulunca
// monitor EŞİK KADAR ardışık hatadan SONRA down oluyor, düzelince kurtarma
// eşiğinden sonra up. Saf fonksiyonun testleri kararı ölçüyor; bu test kararın
// gerçekten SATIRA yazıldığını ve sayaçların turlar arasında taşındığını
// ölçüyor.
func TestStateMachineEndToEndOutageAndRecovery(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	var healthy atomic.Bool
	healthy.Store(true)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	monitor := createMonitor(t, pool, orgID,
		withURL(server.URL+"/health"), withInterval(30), withTimeout(5000))

	svc := NewService(queries, checker.New(safedial.Policy{AllowPrivate: true}), results.NewService(queries, discardLogger()),
		incidents.NewService(pool, queries, discardLogger()), &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 2})

	// runCheck, monitor'ü hemen sıraya alıp bir tur çalıştırır (gerçek zamanı
	// beklememek için: aralık 30 saniye).
	runCheck := func() store.Monitor {
		t.Helper()

		setNextCheckAt(t, pool, monitor.ID, time.Now().Add(-time.Second))
		if _, err := svc.RunDue(t.Context()); err != nil {
			t.Fatalf("tur başarısız: %v", err)
		}

		return getMonitor(t, pool, monitor.ID)
	}

	// İlk başarılı kontrol: pending → up (kurtarma eşiği "kesintiden çıkış"
	// kuralı, "ilk kez ölçüldü" değil).
	after := runCheck()
	if after.CurrentStatus != store.MonitorStatusUp {
		t.Fatalf("ilk kontrolden sonra durum = %q, beklenen up", after.CurrentStatus)
	}
	firstUpAt := after.StatusChangedAt

	// İkinci başarı: durum aynı, status_changed_at KORUNUYOR.
	after = runCheck()
	if !after.StatusChangedAt.Equal(firstUpAt) {
		t.Errorf("durum değişmediği hâlde status_changed_at tazelendi: %s → %s",
			firstUpAt, after.StatusChangedAt)
	}
	if after.ConsecutiveSuccesses != 2 {
		t.Errorf("ardışık başarı = %d, beklenen 2", after.ConsecutiveSuccesses)
	}

	// Hedef bozuluyor: eşik 3 olduğu için ilk iki hata durumu DEĞİŞTİRMEMELİ.
	healthy.Store(false)

	after = runCheck()
	if after.CurrentStatus != store.MonitorStatusUp || after.ConsecutiveFailures != 1 {
		t.Fatalf("1. hata: durum = %q, ardışık hata = %d", after.CurrentStatus, after.ConsecutiveFailures)
	}

	after = runCheck()
	if after.CurrentStatus != store.MonitorStatusUp || after.ConsecutiveFailures != 2 {
		t.Fatalf("2. hata: durum = %q, ardışık hata = %d", after.CurrentStatus, after.ConsecutiveFailures)
	}

	after = runCheck()
	if after.CurrentStatus != store.MonitorStatusDown {
		t.Fatalf("3. hata: durum = %q, beklenen down", after.CurrentStatus)
	}
	if !after.StatusChangedAt.After(firstUpAt) {
		t.Errorf("kesintide status_changed_at güncellenmedi: %s", after.StatusChangedAt)
	}

	// Hedef düzeliyor: kurtarma eşiği 2, yani ilk başarı yetmiyor.
	healthy.Store(true)

	after = runCheck()
	if after.CurrentStatus != store.MonitorStatusDown {
		t.Fatalf("1. kurtarma: durum = %q, hâlâ down olmalıydı", after.CurrentStatus)
	}
	if after.ConsecutiveSuccesses != 1 || after.ConsecutiveFailures != 0 {
		t.Errorf("sayaçlar = %d başarı / %d hata", after.ConsecutiveSuccesses, after.ConsecutiveFailures)
	}

	after = runCheck()
	if after.CurrentStatus != store.MonitorStatusUp {
		t.Fatalf("2. kurtarma: durum = %q, beklenen up", after.CurrentStatus)
	}

	// Kesinti boyunca her kontrol satırı yazılmaya devam etti (durum makinesi
	// zaman serisini etkilemiyor).
	if rows := listChecks(t, pool, monitor.ID); len(rows) != 7 {
		t.Errorf("kontrol satırı = %d, beklenen 7", len(rows))
	}
}

// Monitor bazında eşik: 1/1 ayarlı bir monitor İLK hatada down, İLK başarıda
// up oluyor — kararın kaynağı satırdaki eşikler.
func TestPerMonitorThresholdsAreHonoured(t *testing.T) {
	pool := newTestPool(t)
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	var healthy atomic.Bool
	healthy.Store(false)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	monitor := createMonitor(t, pool, orgID,
		withURL(server.URL+"/health"), withInterval(30), withTimeout(5000), withThresholds(1, 1))

	svc := NewService(queries, checker.New(safedial.Policy{AllowPrivate: true}), results.NewService(queries, discardLogger()),
		incidents.NewService(pool, queries, discardLogger()), &fakeBroadcaster{}, discardLogger(), Options{MaxConcurrentChecks: 1})

	if _, err := svc.RunDue(t.Context()); err != nil {
		t.Fatalf("tur başarısız: %v", err)
	}

	if after := getMonitor(t, pool, monitor.ID); after.CurrentStatus != store.MonitorStatusDown {
		t.Fatalf("eşik 1 iken ilk hatada durum = %q, beklenen down", after.CurrentStatus)
	}

	healthy.Store(true)
	setNextCheckAt(t, pool, monitor.ID, time.Now().Add(-time.Second))

	if _, err := svc.RunDue(t.Context()); err != nil {
		t.Fatalf("tur başarısız: %v", err)
	}

	if after := getMonitor(t, pool, monitor.ID); after.CurrentStatus != store.MonitorStatusUp {
		t.Errorf("eşik 1 iken ilk başarıda durum = %q, beklenen up", after.CurrentStatus)
	}
}
