package monitors

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu testler GERÇEK Postgres'e karşı koşuyor. Sebep sahte bir store ile
// ölçülemeyecek iki şey: (1) org izolasyonu SQL'in WHERE koşuluna dayanıyor —
// sahte store'da o koşulu ben yazardım, test kendi kopyasını doğrulardı;
// (2) şemadaki CHECK kısıtlarının gerçekten son hakem olduğu.

func newTestService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()

	return NewService(pool, store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// createOrg, testlere sahibi olan bir organizasyon verir.
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

	// TEST ORGANİZASYONU **PRO** PLANDA (8.5): bu dosyanın testleri 30-60
	// saniyelik aralıklar kullanıyor ve konuları kota değil (doğrulama, org
	// izolasyonu, kısmi güncelleme). FREE planın 300 saniyelik alt sınırı
	// hepsini alakasız bir sebeple düşürürdü. Kotanın KENDİSİ quota_test.go'da
	// ölçülüyor ve orada plan bilinçli olarak FREE.
	if _, err := queries.UpdateOrganizationPlan(t.Context(), store.UpdateOrganizationPlanParams{
		ID:   org.ID,
		Plan: store.OrgPlanPro,
	}); err != nil {
		t.Fatalf("plan ayarlanamadı: %v", err)
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

func TestCreatePersistsSettings(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	in := validSettings()
	in.URL = "HTTPS://Example.com:443/health"

	monitor, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if monitor.OrgID != orgID {
		t.Errorf("org_id = %s, beklenen %s", monitor.OrgID, orgID)
	}
	// Normalleştirilmiş hâliyle saklanmalı.
	if monitor.Url != "https://example.com/health" {
		t.Errorf("url = %q", monitor.Url)
	}
	// İlk kontrol yapılana kadar 'pending'.
	if monitor.CurrentStatus != store.MonitorStatusPending {
		t.Errorf("durum = %q, beklenen pending", monitor.CurrentStatus)
	}
}

func TestCreateInactiveStartsPaused(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	in := validSettings()
	in.IsActive = false

	monitor, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if monitor.CurrentStatus != store.MonitorStatusPaused {
		t.Errorf("durum = %q, beklenen paused", monitor.CurrentStatus)
	}
}

// Doğrulama SERVİSTE: geçersiz ayar veritabanına hiç ulaşmıyor.
func TestCreateRejectsInvalidSettings(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	in := validSettings()
	in.URL = "example.com"

	if _, err := svc.Create(t.Context(), orgID, in); err == nil {
		t.Fatal("hata bekleniyordu")
	} else {
		var v *ValidationError
		if !errors.As(err, &v) {
			t.Fatalf("doğrulama hatası bekleniyordu: %v", err)
		}
	}

	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM monitors`).Scan(&count); err != nil {
		t.Fatalf("sayım okunamadı: %v", err)
	}
	if count != 0 {
		t.Errorf("satır yazılmamalıydı, %d satır var", count)
	}
}

// ORG İZOLASYONU: başka organizasyonun monitor'ü ne okunabilir ne
// güncellenebilir ne silinebilir — üçünde de "yok" cevabı.
func TestOtherOrgCannotReachMonitor(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgA := createOrg(t, pool)
	orgB := createOrg(t, pool)

	monitor, err := svc.Create(t.Context(), orgA, validSettings())
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	if _, err := svc.Get(t.Context(), orgB, monitor.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: ErrNotFound bekleniyordu, gelen %v", err)
	}

	name := "ele geçirildi"
	if _, err := svc.Update(t.Context(), orgB, monitor.ID, Patch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: ErrNotFound bekleniyordu, gelen %v", err)
	}

	if err := svc.Delete(t.Context(), orgB, monitor.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete: ErrNotFound bekleniyordu, gelen %v", err)
	}

	list, err := svc.List(t.Context(), orgB)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}
	if len(list.Monitors) != 0 {
		t.Errorf("B'nin listesi boş olmalıydı, %d kayıt var", len(list.Monitors))
	}

	// A'nın kaydı bozulmamış olmalı.
	after, err := svc.Get(t.Context(), orgA, monitor.ID)
	if err != nil {
		t.Fatalf("A kendi monitor'ünü okuyamadı: %v", err)
	}
	if after.Name != monitor.Name {
		t.Errorf("ad değişmiş: %q", after.Name)
	}
}

func TestUpdateMergesOnlyGivenFields(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	monitor, err := svc.Create(t.Context(), orgID, validSettings())
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	interval := int32(120)
	updated, err := svc.Update(t.Context(), orgID, monitor.ID, Patch{CheckIntervalSeconds: &interval})
	if err != nil {
		t.Fatalf("güncellenemedi: %v", err)
	}

	if updated.CheckIntervalSeconds != 120 {
		t.Errorf("aralık = %d", updated.CheckIntervalSeconds)
	}
	if updated.Name != monitor.Name || updated.Url != monitor.Url || updated.TimeoutMs != monitor.TimeoutMs {
		t.Errorf("dokunulmayan alanlar değişti: %+v", updated)
	}
	if !updated.UpdatedAt.After(monitor.UpdatedAt) && !updated.UpdatedAt.Equal(monitor.UpdatedAt) {
		t.Errorf("updated_at geriye gitti")
	}
}

// Birleşmiş hâlin doğrulaması: tek başına geçerli bir timeout, mevcut
// aralıkla birlikte geçersiz olabilir.
func TestUpdateValidatesMergedResult(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	in := validSettings()
	in.CheckIntervalSeconds = 30
	in.TimeoutMs = 5000

	monitor, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	timeout := int32(45000) // tek başına geçerli, 30 sn aralıkla geçersiz
	_, err = svc.Update(t.Context(), orgID, monitor.ID, Patch{TimeoutMs: &timeout})

	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("doğrulama hatası bekleniyordu: %v", err)
	}
	if v.Problems[0].Field != "timeoutMs" {
		t.Errorf("alan = %q", v.Problems[0].Field)
	}
}

// "Yavaş" eşiği ÜÇ DURUMLU: dokunma / ayarla / kaldır.
func TestUpdateDegradedThresholdIsThreeState(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	in := validSettings()
	in.DegradedResponseMs = ptr(int32(500))

	monitor, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}
	if monitor.DegradedResponseMs == nil || *monitor.DegradedResponseMs != 500 {
		t.Fatalf("eşik = %v", monitor.DegradedResponseMs)
	}

	// Alan gönderilmedi → DOKUNULMUYOR.
	name := "Yeni ad"
	untouched, err := svc.Update(t.Context(), orgID, monitor.ID, Patch{Name: &name})
	if err != nil {
		t.Fatalf("güncellenemedi: %v", err)
	}
	if untouched.DegradedResponseMs == nil || *untouched.DegradedResponseMs != 500 {
		t.Errorf("eşik değişti: %v", untouched.DegradedResponseMs)
	}

	// null gönderildi → KALDIRILIYOR.
	cleared, err := svc.Update(t.Context(), orgID, monitor.ID,
		Patch{SetDegradedResponseMs: true, DegradedResponseMs: nil})
	if err != nil {
		t.Fatalf("eşik kaldırılamadı: %v", err)
	}
	if cleared.DegradedResponseMs != nil {
		t.Errorf("eşik = %v, nil olmalıydı", *cleared.DegradedResponseMs)
	}

	// Yalnızca eşiği göndermek de geçerli bir güncelleme (boş patch değil).
	restored, err := svc.Update(t.Context(), orgID, monitor.ID,
		Patch{SetDegradedResponseMs: true, DegradedResponseMs: ptr(int32(750))})
	if err != nil {
		t.Fatalf("eşik yazılamadı: %v", err)
	}
	if restored.DegradedResponseMs == nil || *restored.DegradedResponseMs != 750 {
		t.Errorf("eşik = %v", restored.DegradedResponseMs)
	}
}

func TestUpdateRejectsEmptyPatch(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	monitor, err := svc.Create(t.Context(), orgID, validSettings())
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	var v *ValidationError
	if _, err := svc.Update(t.Context(), orgID, monitor.ID, Patch{}); !errors.As(err, &v) {
		t.Fatalf("doğrulama hatası bekleniyordu: %v", err)
	}
}

// Duraklat/başlat, denormalize durumu tazeliyor; aktiflik değişmediğinde
// duruma DOKUNULMUYOR (sahibi 3.3'ün durum makinesi).
func TestUpdateStatusFollowsActiveFlag(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	monitor, err := svc.Create(t.Context(), orgID, validSettings())
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	// 3.3'ün yazacağı durumu taklit et.
	if _, err := pool.Exec(t.Context(),
		`UPDATE monitors SET current_status = 'up' WHERE id = $1`, monitor.ID); err != nil {
		t.Fatalf("durum yazılamadı: %v", err)
	}

	name := "Yeni ad"
	renamed, err := svc.Update(t.Context(), orgID, monitor.ID, Patch{Name: &name})
	if err != nil {
		t.Fatalf("güncellenemedi: %v", err)
	}
	if renamed.CurrentStatus != store.MonitorStatusUp {
		t.Errorf("ad değişikliği durumu bozdu: %q", renamed.CurrentStatus)
	}

	inactive := false
	paused, err := svc.Update(t.Context(), orgID, monitor.ID, Patch{IsActive: &inactive})
	if err != nil {
		t.Fatalf("duraklatılamadı: %v", err)
	}
	if paused.CurrentStatus != store.MonitorStatusPaused {
		t.Errorf("durum = %q, beklenen paused", paused.CurrentStatus)
	}

	active := true
	resumed, err := svc.Update(t.Context(), orgID, monitor.ID, Patch{IsActive: &active})
	if err != nil {
		t.Fatalf("başlatılamadı: %v", err)
	}
	// Eski duruma DEĞİL 'pending'e dönüyor: duraklatma süresince ne olduğu
	// bilinmiyor.
	if resumed.CurrentStatus != store.MonitorStatusPending {
		t.Errorf("durum = %q, beklenen pending", resumed.CurrentStatus)
	}
}

func TestDeleteRemovesMonitor(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	monitor, err := svc.Create(t.Context(), orgID, validSettings())
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	if err := svc.Delete(t.Context(), orgID, monitor.ID); err != nil {
		t.Fatalf("silinemedi: %v", err)
	}

	// İkinci silme "yok" olmalı — silme idempotent DEĞİL, bilerek: istemci
	// "sildim mi silmedim mi" sorusunun cevabını alsın.
	if err := svc.Delete(t.Context(), orgID, monitor.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("ikinci silme: ErrNotFound bekleniyordu, gelen %v", err)
	}
}

// Organizasyon silinince monitor'leri de gidiyor (ON DELETE CASCADE).
func TestDeletingOrgCascadesToMonitors(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	svc := newTestService(t, pool)
	orgID := createOrg(t, pool)

	if _, err := svc.Create(t.Context(), orgID, validSettings()); err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	if _, err := pool.Exec(t.Context(), `DELETE FROM organizations WHERE id = $1`, orgID); err != nil {
		t.Fatalf("organizasyon silinemedi: %v", err)
	}

	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM monitors`).Scan(&count); err != nil {
		t.Fatalf("sayım okunamadı: %v", err)
	}
	if count != 0 {
		t.Errorf("monitor kalmamalıydı, %d satır var", count)
	}
}

// ŞEMA SON HAKEM: doğrulama atlanarak (servis yerine doğrudan sorgu) yazılan
// bozuk bir kayıt veritabanı tarafından reddedilmeli. Uygulamadaki sınırların
// kopyası şemada bu yüzden duruyor.
func TestSchemaRejectsInvalidRowWhenValidationBypassed(t *testing.T) {
	pool := testdb.Pool(t, "monitors")
	orgID := createOrg(t, pool)
	queries := store.New(pool)

	base := store.CreateMonitorParams{
		OrgID:                orgID,
		Name:                 "API",
		Url:                  "https://example.com",
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: 60,
		TimeoutMs:            10000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		FailureThreshold:     DefaultFailureThreshold,
		RecoveryThreshold:    DefaultRecoveryThreshold,
		IsActive:             true,
		CurrentStatus:        store.MonitorStatusPending,
	}

	tests := []struct {
		name  string
		bozar func(*store.CreateMonitorParams)
	}{
		{"http olmayan url", func(p *store.CreateMonitorParams) { p.Url = "ftp://example.com" }},
		{"boş ad", func(p *store.CreateMonitorParams) { p.Name = "" }},
		{"çok sık aralık", func(p *store.CreateMonitorParams) { p.CheckIntervalSeconds = 5 }},
		{"aralıktan uzun timeout", func(p *store.CreateMonitorParams) {
			p.CheckIntervalSeconds = 30
			p.TimeoutMs = 60000
		}},
		{"ters status aralığı", func(p *store.CreateMonitorParams) {
			p.ExpectedStatusMin = 500
			p.ExpectedStatusMax = 200
		}},
		{"zaman aşımından büyük degraded eşiği", func(p *store.CreateMonitorParams) {
			p.TimeoutMs = 5000
			p.DegradedResponseMs = ptr(int32(5001))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := base
			tt.bozar(&params)

			if _, err := queries.CreateMonitor(t.Context(), params); err == nil {
				t.Error("veritabanı kaydı reddetmeliydi")
			}
		})
	}
}
