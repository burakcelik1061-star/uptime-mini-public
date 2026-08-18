package monitors

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// PLAN KOTASI (8.5) — gerçek veritabanına karşı.
//
// Bu dosyanın organizasyonları bilinçli olarak FREE planda kalıyor
// (service_test.go'daki yardımcı PRO'ya çekiyor, çünkü oradaki testlerin
// konusu kota değil). Buradaki testlerin konusu tam olarak sınırın kendisi.

// freeOrg, FREE planda bir organizasyon döner.
func freeOrg(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()

	orgID := createOrg(t, pool)

	if _, err := store.New(pool).UpdateOrganizationPlan(t.Context(), store.UpdateOrganizationPlanParams{
		ID:   orgID,
		Plan: store.OrgPlanFree,
	}); err != nil {
		t.Fatalf("plan FREE'ye çekilemedi: %v", err)
	}

	return orgID
}

// freeSettings, FREE planın alt sınırına uyan geçerli bir monitor.
func freeSettings(name string) Settings {
	return Settings{
		Name:                 name,
		URL:                  "https://example.com/" + name,
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: plans.For(store.OrgPlanFree).MinCheckIntervalSeconds,
		TimeoutMs:            10000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		FailureThreshold:     3,
		RecoveryThreshold:    2,
		IsActive:             true,
	}
}

// MONITOR SAYISI SINIRI: FREE planda 5'ten fazlası yazılamıyor.
func TestCreateStopsAtPlanMonitorLimit(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := freeOrg(t, pool)

	limit := plans.For(store.OrgPlanFree).MaxMonitors

	for i := range limit {
		if _, err := svc.Create(t.Context(), orgID, freeSettings(fmt.Sprintf("monitor-%d", i))); err != nil {
			t.Fatalf("%d. monitor reddedildi: %v", i+1, err)
		}
	}

	_, err := svc.Create(t.Context(), orgID, freeSettings("bir-fazla"))

	var limitErr *ErrMonitorLimitReached
	if !errors.As(err, &limitErr) {
		t.Fatalf("hata = %v, beklenen ErrMonitorLimitReached", err)
	}
	if limitErr.Limit != limit {
		t.Errorf("sınır = %d, beklenen %d", limitErr.Limit, limit)
	}

	// Reddedilen istek SATIR YAZMAMALI.
	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM monitors WHERE org_id = $1`, orgID).Scan(&count); err != nil {
		t.Fatalf("sayım yapılamadı: %v", err)
	}
	if count != limit {
		t.Errorf("monitor sayısı = %d, beklenen %d", count, limit)
	}
}

// DURAKLATILMIŞ MONITOR DE SAYILIYOR: saymasaydık, kotasını doldurmuş biri
// hepsini duraklatıp sınırsız monitor tanımlayabilirdi.
func TestPausedMonitorsCountTowardTheLimit(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := freeOrg(t, pool)

	limit := plans.For(store.OrgPlanFree).MaxMonitors

	for i := range limit {
		in := freeSettings(fmt.Sprintf("monitor-%d", i))
		in.IsActive = false

		if _, err := svc.Create(t.Context(), orgID, in); err != nil {
			t.Fatalf("%d. monitor reddedildi: %v", i+1, err)
		}
	}

	if _, err := svc.Create(t.Context(), orgID, freeSettings("bir-fazla")); err == nil {
		t.Fatal("duraklatılmış monitor'ler kotaya sayılmadı")
	}
}

// PLAN YÜKSELİNCE SINIR AÇILIYOR — mock ödemenin var olma sebebi bu:
// yalnızca duvarı gösteren bir demo, yükseltmenin işe yaradığını gösteremez.
func TestUpgradingPlanRaisesTheLimit(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	queries := store.New(pool)
	orgID := freeOrg(t, pool)

	for i := range plans.For(store.OrgPlanFree).MaxMonitors {
		if _, err := svc.Create(t.Context(), orgID, freeSettings(fmt.Sprintf("monitor-%d", i))); err != nil {
			t.Fatalf("%d. monitor reddedildi: %v", i+1, err)
		}
	}

	if _, err := svc.Create(t.Context(), orgID, freeSettings("engellenen")); err == nil {
		t.Fatal("FREE planda sınır uygulanmadı")
	}

	if _, err := queries.UpdateOrganizationPlan(t.Context(), store.UpdateOrganizationPlanParams{
		ID:   orgID,
		Plan: store.OrgPlanPro,
	}); err != nil {
		t.Fatalf("plan yükseltilemedi: %v", err)
	}

	if _, err := svc.Create(t.Context(), orgID, freeSettings("yukseltmeden-sonra")); err != nil {
		t.Fatalf("PRO planda monitor eklenemedi: %v", err)
	}
}

// ARALIK SINIRI: FREE planda 30 saniye reddediliyor (şema izin verse bile).
func TestCreateRejectsIntervalBelowPlanMinimum(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := freeOrg(t, pool)

	in := freeSettings("cok-sik")
	in.CheckIntervalSeconds = 30 // şemanın (00005) izin verdiği alt sınır

	_, err := svc.Create(t.Context(), orgID, in)

	var intervalErr *ErrIntervalBelowPlan
	if !errors.As(err, &intervalErr) {
		t.Fatalf("hata = %v, beklenen ErrIntervalBelowPlan", err)
	}
	if intervalErr.Minimum != plans.For(store.OrgPlanFree).MinCheckIntervalSeconds {
		t.Errorf("alt sınır = %d", intervalErr.Minimum)
	}
}

// GÜNCELLEMEDE DE ARALIK DENETLENİYOR — yoksa kotayı atlatmak iki adımlık
// bir iş olurdu: 300 saniyeyle aç, sonra 30 saniyeye çek.
func TestUpdateRejectsIntervalBelowPlanMinimum(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := freeOrg(t, pool)

	monitor, err := svc.Create(t.Context(), orgID, freeSettings("once-uygun"))
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	interval := int32(30)
	_, err = svc.Update(t.Context(), orgID, monitor.ID, Patch{
		CheckIntervalSeconds: &interval,
	})

	var intervalErr *ErrIntervalBelowPlan
	if !errors.As(err, &intervalErr) {
		t.Fatalf("hata = %v, beklenen ErrIntervalBelowPlan", err)
	}
}

// KOTASI DOLU BİR ORGANİZASYON MEVCUT MONITOR'ÜNÜ DÜZENLEYEBİLMELİ:
// sınır YENİ bir şey eklemeyi durduruyor, var olanı yönetmeyi değil.
func TestUpdateWorksWhenOrganizationIsAtItsLimit(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := freeOrg(t, pool)

	var first store.Monitor
	for i := range plans.For(store.OrgPlanFree).MaxMonitors {
		monitor, err := svc.Create(t.Context(), orgID, freeSettings(fmt.Sprintf("monitor-%d", i)))
		if err != nil {
			t.Fatalf("%d. monitor reddedildi: %v", i+1, err)
		}
		if i == 0 {
			first = monitor
		}
	}

	name := "yeni-ad"
	if _, err := svc.Update(t.Context(), orgID, first.ID, Patch{Name: &name}); err != nil {
		t.Fatalf("kotası dolu organizasyon monitor'ünü düzenleyemedi: %v", err)
	}
}

// EŞZAMANLI OLUŞTURMA KOTAYI AŞAMIYOR.
//
// "Önce say, sınırın altındaysa yaz" iki eşzamanlı istekte ikisini de
// geçirirdi. Hakem satır kilidi (LockOrganizationForQuota) — 1.3/4.1/6.3'te
// UNIQUE kısıtının oynadığı rolün, sayımla ifade edilemeyen hâli.
func TestConcurrentCreatesCannotExceedTheLimit(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := freeOrg(t, pool)

	limit := plans.For(store.OrgPlanFree).MaxMonitors

	// Sınırın iki katı kadar istek AYNI ANDA.
	attempts := limit * 2

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
	)

	start := make(chan struct{})

	for i := range attempts {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()
			<-start

			if _, err := svc.Create(t.Context(), orgID, freeSettings(fmt.Sprintf("yaris-%d", i))); err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if created != limit {
		t.Errorf("yazılan monitor = %d, beklenen %d (kota aşıldı)", created, limit)
	}

	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM monitors WHERE org_id = $1`, orgID).Scan(&count); err != nil {
		t.Fatalf("sayım yapılamadı: %v", err)
	}
	if count != limit {
		t.Errorf("veritabanında %d monitor var, sınır %d", count, limit)
	}
}
