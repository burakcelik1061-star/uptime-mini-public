package maintenance

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu paketin testleri GERÇEK VERİTABANINA karşı: ölçülen şeyler bir
// transaction'ın geri alınması (yabancı monitor gönderilince pencere KALMAMALI)
// ve org izolasyonu — ikisi de sahte bir store ile ölçülemez.

func newService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()

	return NewService(pool, store.New(pool), slog.New(slog.DiscardHandler))
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

func createMonitor(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, name string) store.Monitor {
	t.Helper()

	monitor, err := store.New(pool).CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                orgID,
		Name:                 name,
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

// futureInput, yarın başlayan iki saatlik bir pencere.
func futureInput(monitorIDs ...uuid.UUID) Input {
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)

	return Input{
		Title:      "Veritabanı yükseltmesi",
		StartsAt:   start,
		EndsAt:     start.Add(2 * time.Hour),
		MonitorIDs: monitorIDs,
	}
}

func TestCreateStoresWindowWithMonitors(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	api := createMonitor(t, pool, orgID, "API")
	web := createMonitor(t, pool, orgID, "Web")

	description := "  Yükseltme sırasında kısa kesintiler olabilir.  "
	in := futureInput(api.ID, web.ID)
	in.Description = &description

	window, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("pencere oluşturulamadı: %v", err)
	}

	if window.Window.Title != "Veritabanı yükseltmesi" {
		t.Errorf("başlık = %q", window.Window.Title)
	}
	if window.Window.Description == nil || *window.Window.Description !=
		"Yükseltme sırasında kısa kesintiler olabilir." {
		t.Errorf("açıklama kırpılmadı: %v", window.Window.Description)
	}
	if len(window.Monitors) != 2 {
		t.Fatalf("servis sayısı = %d, beklenen 2", len(window.Monitors))
	}
	// Sıralama ada göre.
	if window.Monitors[0].Name != "API" || window.Monitors[1].Name != "Web" {
		t.Errorf("servisler = %+v", window.Monitors)
	}
}

// Boş açıklama ile "açıklama yok" aynı şey: veritabanında iki gösterimi olmasın.
func TestCreateNormalizesBlankDescriptionToNull(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	blank := "   "
	in := futureInput(monitor.ID)
	in.Description = &blank

	window, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("pencere oluşturulamadı: %v", err)
	}

	if window.Window.Description != nil {
		t.Errorf("açıklama = %q, beklenen NULL", *window.Window.Description)
	}
}

// YABANCI MONITOR → İŞLEM TÜMDEN GERİ ALINIYOR: yarım kapsamlı bir pencere
// bırakmak, kullanıcının kapsadığını sandığı bir servisi bakım dışında
// bırakırdı (ve bakım sırasında incident açılırdı).
func TestCreateRollsBackWhenMonitorBelongsToAnotherOrg(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	otherOrg := createOrg(t, pool)

	mine := createMonitor(t, pool, orgID, "API")
	foreign := createMonitor(t, pool, otherOrg, "Yabancı")

	_, err := svc.Create(t.Context(), orgID, futureInput(mine.ID, foreign.ID))

	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("hata = %v, beklenen ValidationError", err)
	}
	if validation.Problems[0].Field != "monitorIds" {
		t.Errorf("alan = %q, beklenen monitorIds", validation.Problems[0].Field)
	}

	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM maintenance_windows WHERE org_id = $1`, orgID).Scan(&count); err != nil {
		t.Fatalf("pencereler sayılamadı: %v", err)
	}
	if count != 0 {
		t.Fatalf("geri alınmamış pencere sayısı = %d, beklenen 0", count)
	}
}

func TestCreateRejectsUnknownMonitor(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)

	if _, err := svc.Create(t.Context(), orgID, futureInput(uuid.New())); err == nil {
		t.Fatal("olmayan servis kabul edildi")
	}
}

// GEÇMİŞE DÖNÜK PENCERE REDDEDİLİYOR: bitmiş bir aralığı sonradan "bakım" ilan
// etmek uptime geçmişini yeniden yazmak demek.
func TestValidateRejectsPastWindow(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	in := futureInput(monitor.ID)
	in.StartsAt = time.Now().UTC().Add(-3 * time.Hour)
	in.EndsAt = time.Now().UTC().Add(-time.Hour)

	var validation *ValidationError
	if _, err := svc.Create(t.Context(), orgID, in); !errors.As(err, &validation) {
		t.Fatalf("hata = %v, beklenen ValidationError", err)
	}
	if validation.Problems[0].Field != "endsAt" {
		t.Errorf("alan = %q, beklenen endsAt", validation.Problems[0].Field)
	}
}

// AMA DEVAM EDEN bir bakımı şimdi ilan etmek meşru: başlangıç geçmişte
// olabiliyor (bitiş gelecekte olduğu sürece).
func TestValidateAllowsAlreadyStartedWindow(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	in := futureInput(monitor.ID)
	in.StartsAt = time.Now().UTC().Add(-30 * time.Minute)
	in.EndsAt = time.Now().UTC().Add(30 * time.Minute)

	if _, err := svc.Create(t.Context(), orgID, in); err != nil {
		t.Fatalf("devam eden bakım reddedildi: %v", err)
	}
}

func TestValidateFieldRules(t *testing.T) {
	svc := &Service{now: func() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) }}
	base := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	monitorID := uuid.New()

	cases := []struct {
		name  string
		in    Input
		field string
	}{
		{
			name:  "başlık zorunlu",
			in:    Input{StartsAt: base, EndsAt: base.Add(time.Hour), MonitorIDs: []uuid.UUID{monitorID}},
			field: "title",
		},
		{
			name: "bitiş başlangıçtan önce",
			in: Input{
				Title: "x", StartsAt: base, EndsAt: base.Add(-time.Hour),
				MonitorIDs: []uuid.UUID{monitorID},
			},
			field: "endsAt",
		},
		{
			name: "çok kısa",
			in: Input{
				Title: "x", StartsAt: base, EndsAt: base.Add(30 * time.Second),
				MonitorIDs: []uuid.UUID{monitorID},
			},
			field: "endsAt",
		},
		{
			name: "çok uzun",
			in: Input{
				Title: "x", StartsAt: base, EndsAt: base.Add(MaxDuration + time.Hour),
				MonitorIDs: []uuid.UUID{monitorID},
			},
			field: "endsAt",
		},
		{
			name:  "servis seçilmemiş",
			in:    Input{Title: "x", StartsAt: base, EndsAt: base.Add(time.Hour)},
			field: "monitorIds",
		},
		{
			name: "aynı servis iki kez",
			in: Input{
				Title: "x", StartsAt: base, EndsAt: base.Add(time.Hour),
				MonitorIDs: []uuid.UUID{monitorID, monitorID},
			},
			field: "monitorIds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var validation *ValidationError
			if err := svc.Validate(Normalize(tc.in)); !errors.As(err, &validation) {
				t.Fatalf("hata = %v, beklenen ValidationError", err)
			}

			if validation.Problems[0].Field != tc.field {
				t.Errorf("alan = %q, beklenen %q", validation.Problems[0].Field, tc.field)
			}
		})
	}
}

// Şema son hakem: doğrulama atlansa bile ters aralık giremiyor.
func TestSchemaRejectsInvertedRange(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	orgID := createOrg(t, pool)

	_, err := pool.Exec(t.Context(),
		`INSERT INTO maintenance_windows (org_id, title, starts_at, ends_at)
		 VALUES ($1, 'x', now() + interval '2 hours', now() + interval '1 hour')`, orgID)
	if err == nil {
		t.Fatal("ters aralık kabul edildi, beklenen: kısıt ihlali")
	}
}

func TestListReturnsWindowsNewestFirst(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	first := futureInput(monitor.ID)
	second := futureInput(monitor.ID)
	second.Title = "İkinci"
	second.StartsAt = first.StartsAt.Add(48 * time.Hour)
	second.EndsAt = second.StartsAt.Add(time.Hour)

	if _, err := svc.Create(t.Context(), orgID, first); err != nil {
		t.Fatalf("ilk pencere: %v", err)
	}
	if _, err := svc.Create(t.Context(), orgID, second); err != nil {
		t.Fatalf("ikinci pencere: %v", err)
	}

	windows, err := svc.List(t.Context(), orgID, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}

	if len(windows) != 2 {
		t.Fatalf("pencere sayısı = %d, beklenen 2", len(windows))
	}
	if windows[0].Window.Title != "İkinci" {
		t.Errorf("ilk satır = %q, beklenen \"İkinci\" (yeniden eskiye)", windows[0].Window.Title)
	}
	if len(windows[0].Monitors) != 1 || windows[0].Monitors[0].ID != monitor.ID {
		t.Errorf("servisler = %+v", windows[0].Monitors)
	}
}

// ORG İZOLASYONU: başka organizasyonun penceresi listede yok, silinemiyor.
func TestWindowsAreIsolatedByOrg(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	otherOrg := createOrg(t, pool)

	monitor := createMonitor(t, pool, orgID, "API")
	window, err := svc.Create(t.Context(), orgID, futureInput(monitor.ID))
	if err != nil {
		t.Fatalf("pencere oluşturulamadı: %v", err)
	}

	windows, err := svc.List(t.Context(), otherOrg, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("yabancı organizasyon %d pencere gördü", len(windows))
	}

	if err := svc.Delete(t.Context(), otherOrg, window.Window.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}
}

func TestDeleteRemovesUpcomingWindow(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	window, err := svc.Create(t.Context(), orgID, futureInput(monitor.ID))
	if err != nil {
		t.Fatalf("pencere oluşturulamadı: %v", err)
	}

	if err := svc.Delete(t.Context(), orgID, window.Window.ID); err != nil {
		t.Fatalf("pencere silinemedi: %v", err)
	}

	windows, err := svc.List(t.Context(), orgID, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("silinen pencere hâlâ listede: %+v", windows)
	}
}

// BAŞLAMIŞ PENCERE SİLİNEMİYOR: geçmişi yeniden yazmamak için.
func TestDeleteRejectsStartedWindow(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	in := futureInput(monitor.ID)
	in.StartsAt = time.Now().UTC().Add(-10 * time.Minute)
	in.EndsAt = time.Now().UTC().Add(time.Hour)

	window, err := svc.Create(t.Context(), orgID, in)
	if err != nil {
		t.Fatalf("pencere oluşturulamadı: %v", err)
	}

	if err := svc.Delete(t.Context(), orgID, window.Window.ID); !errors.Is(err, ErrStarted) {
		t.Fatalf("hata = %v, beklenen ErrStarted", err)
	}

	// Kayıt duruyor olmalı.
	windows, err := svc.List(t.Context(), orgID, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("pencere sayısı = %d, beklenen 1", len(windows))
	}
}

func TestDeleteUnknownWindow(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	if err := svc.Delete(t.Context(), createOrg(t, pool), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}
}

// Monitor silinince bağ satırı düşüyor, pencere kalıyor (bakım kaydı bir
// planlama; kapsadığı servis kaldırılmış olabilir).
func TestWindowSurvivesMonitorDeletion(t *testing.T) {
	pool := testdb.Pool(t, "maintenance")
	svc := newService(t, pool)

	orgID := createOrg(t, pool)
	monitor := createMonitor(t, pool, orgID, "API")

	if _, err := svc.Create(t.Context(), orgID, futureInput(monitor.ID)); err != nil {
		t.Fatalf("pencere oluşturulamadı: %v", err)
	}

	if _, err := store.New(pool).DeleteMonitor(t.Context(), store.DeleteMonitorParams{
		ID: monitor.ID, OrgID: orgID,
	}); err != nil {
		t.Fatalf("monitor silinemedi: %v", err)
	}

	windows, err := svc.List(t.Context(), orgID, 0)
	if err != nil {
		t.Fatalf("liste okunamadı: %v", err)
	}

	if len(windows) != 1 {
		t.Fatalf("pencere sayısı = %d, beklenen 1", len(windows))
	}
	if len(windows[0].Monitors) != 0 {
		t.Errorf("servis sayısı = %d, beklenen 0", len(windows[0].Monitors))
	}
}

func TestClampLimit(t *testing.T) {
	cases := map[int32]int32{0: DefaultListLimit, -1: DefaultListLimit, 7: 7, 9999: MaxListLimit}

	for in, want := range cases {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %d, beklenen %d", in, got, want)
		}
	}
}
