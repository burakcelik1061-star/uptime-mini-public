package statuspage

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu paketin testleri GERÇEK VERİTABANINA karşı ve ölçtükleri asıl şey TEK:
// YAYIMLANMAMIŞ BİR SERVİSİN ADI HİÇBİR ALANDAN ÇIKMIYOR MU?
//
// Sahte bir store ile ölçülemez, çünkü kuralı uygulayan şey Go kodu değil
// sorguların kendisi (db/queries/status_pages.sql). En ince yeri 4.1'in
// otomatik incident BAŞLIĞI: monitor adını içeriyor, yani private bir servis
// "başlıkla birlikte" sızabilir.

func newService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()

	return NewService(pool, store.New(pool), slog.New(slog.DiscardHandler))
}

// fixture, bir organizasyon + status page'i olan hazır bir kurulum.
type fixture struct {
	pool    *pgxpool.Pool
	queries *store.Queries
	svc     *Service
	orgID   uuid.UUID
	page    store.StatusPage
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	pool := testdb.Pool(t, "statuspage")
	queries := store.New(pool)

	user, err := queries.CreateUser(t.Context(), store.CreateUserParams{
		Email:        "uye-" + uuid.NewString() + "@example.com",
		PasswordHash: "$argon2id$test",
		Name:         "Test Kullanıcı",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	slug := "org-" + strings.ReplaceAll(uuid.NewString(), "-", "")

	org, err := queries.CreateOrganization(t.Context(), store.CreateOrganizationParams{
		Name: "Acme A.Ş.",
		Slug: slug,
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

	// Üretimde bunu internal/orgs organizasyonla AYNI transaction'da yazıyor;
	// test doğrudan sorguyu çağırıyor (org kurulumunu taklit etmek yerine).
	page, err := queries.CreateStatusPage(t.Context(), store.CreateStatusPageParams{
		OrgID: org.ID,
		Slug:  slug,
	})
	if err != nil {
		t.Fatalf("status page oluşturulamadı: %v", err)
	}

	return &fixture{pool: pool, queries: queries, svc: newService(t, pool), orgID: org.ID, page: page}
}

func (f *fixture) monitor(t *testing.T, name string, status store.MonitorStatus) store.Monitor {
	t.Helper()

	monitor, err := f.queries.CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                f.orgID,
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
		CurrentStatus:        status,
	})
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	return monitor
}

// foreignMonitor, BAŞKA bir organizasyonun monitor'ünü aynı havuzda kurar.
func (f *fixture) foreignMonitor(t *testing.T) store.Monitor {
	t.Helper()

	org, err := f.queries.CreateOrganization(t.Context(), store.CreateOrganizationParams{
		Name: "Yabancı A.Ş.",
		Slug: "org-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	})
	if err != nil {
		t.Fatalf("yabancı organizasyon oluşturulamadı: %v", err)
	}

	monitor, err := f.queries.CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                org.ID,
		Name:                 "Yabancı servis",
		Url:                  "https://example.org/health",
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
		t.Fatalf("yabancı monitor oluşturulamadı: %v", err)
	}

	return monitor
}

func (f *fixture) publish(t *testing.T, monitorIDs ...uuid.UUID) {
	t.Helper()

	if _, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(monitorIDs...)); err != nil {
		t.Fatalf("servisler yayımlanamadı: %v", err)
	}

	published := true
	if _, err := f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{IsPublished: &published}); err != nil {
		t.Fatalf("sayfa yayımlanamadı: %v", err)
	}
}

// ungrouped, gruplamadan yalnızca servis seçen bir girdi üretir (5.1'in
// davranışı; gruplu testler girdiyi elle kuruyor).
func ungrouped(monitorIDs ...uuid.UUID) ServicesInput {
	items := make([]ItemInput, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		items = append(items, ItemInput{MonitorID: id})
	}

	return ServicesInput{Items: items}
}

// autoIncident, 4.1'in otomatik incident'ının test karşılığı.
//
// Başlık monitor ADINI içeriyor — sızıntı testlerinin dayanağı tam olarak bu:
// kayıt gösterilirse ad da gösterilmiş olur.
func (f *fixture) autoIncident(t *testing.T, monitor store.Monitor) uuid.UUID {
	t.Helper()

	var id uuid.UUID

	// Doğrudan INSERT: gerçek OpenAutoIncident sorgusu ham check satırları
	// istiyor ve bu testlerin ölçtüğü şey açma mantığı değil, GÖRÜNÜRLÜK.
	err := f.pool.QueryRow(t.Context(), `
		INSERT INTO incidents (org_id, title, impact, auto_monitor_id, is_auto_created, started_at)
		VALUES ($1, $2, 'major', $3, true, now() - interval '1 hour')
		RETURNING id`,
		f.orgID, monitor.Name+" yanıt vermiyor", monitor.ID).Scan(&id)
	if err != nil {
		t.Fatalf("otomatik incident yazılamadı: %v", err)
	}

	if _, err := f.queries.AddIncidentMonitors(t.Context(), store.AddIncidentMonitorsParams{
		IncidentID: id,
		MonitorIds: []uuid.UUID{monitor.ID},
		OrgID:      f.orgID,
	}); err != nil {
		t.Fatalf("incident servise bağlanamadı: %v", err)
	}

	return id
}

func (f *fixture) manualIncident(t *testing.T, title string, monitorIDs ...uuid.UUID) uuid.UUID {
	t.Helper()

	incident, err := f.queries.CreateIncident(t.Context(), store.CreateIncidentParams{
		OrgID:  f.orgID,
		Title:  title,
		Impact: store.IncidentImpactMinor,
	})
	if err != nil {
		t.Fatalf("manuel incident oluşturulamadı: %v", err)
	}

	if _, err := f.queries.AddIncidentMonitors(t.Context(), store.AddIncidentMonitorsParams{
		IncidentID: incident.ID,
		MonitorIds: monitorIDs,
		OrgID:      f.orgID,
	}); err != nil {
		t.Fatalf("incident servislere bağlanamadı: %v", err)
	}

	return incident.ID
}

func (f *fixture) maintenanceWindow(
	t *testing.T, title string, startsAt, endsAt time.Time, monitorIDs ...uuid.UUID,
) uuid.UUID {
	t.Helper()

	window, err := f.queries.CreateMaintenanceWindow(t.Context(), store.CreateMaintenanceWindowParams{
		OrgID:    f.orgID,
		Title:    title,
		StartsAt: startsAt,
		EndsAt:   endsAt,
	})
	if err != nil {
		t.Fatalf("bakım penceresi oluşturulamadı: %v", err)
	}

	if _, err := f.queries.AddMaintenanceWindowMonitors(t.Context(), store.AddMaintenanceWindowMonitorsParams{
		WindowID:   window.ID,
		MonitorIds: monitorIDs,
		OrgID:      f.orgID,
	}); err != nil {
		t.Fatalf("pencereye servisler bağlanamadı: %v", err)
	}

	return window.ID
}

// --- Yayım kapısı -----------------------------------------------------------

func TestPublicRejectsUnpublishedPage(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)

	// Servisler seçili ama sayfa YAYIMLANMAMIŞ.
	if _, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(api.ID)); err != nil {
		t.Fatalf("servisler yayımlanamadı: %v", err)
	}

	if _, err := f.svc.Public(t.Context(), f.page.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("yayımlanmamış sayfa döndü: %v", err)
	}
}

func TestPublicRejectsUnknownSlug(t *testing.T) {
	f := newFixture(t)

	// Olmayan slug ile yayımlanmamış sayfa AYNI hatayı veriyor: fark üretmek,
	// hangi slug'ların kayıtlı olduğunu dışarıya söylemek olurdu.
	if _, err := f.svc.Public(t.Context(), "boyle-bir-sayfa-yok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bilinmeyen slug için hata = %v", err)
	}
}

func TestPublicNormalizesSlug(t *testing.T) {
	f := newFixture(t)
	f.publish(t)

	// 5.4'te alt alan adı da bu yoldan geçecek.
	page, err := f.svc.Public(t.Context(), "  "+strings.ToUpper(f.page.Slug)+"  ")
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if page.Slug != f.page.Slug {
		t.Errorf("slug = %q, beklenen %q", page.Slug, f.page.Slug)
	}
	if page.Title != "Acme A.Ş." {
		t.Errorf("başlık = %q, beklenen organizasyon adı", page.Title)
	}
}

// --- Sızıntı: servisler -----------------------------------------------------

func TestPublicOnlyReturnsPublishedServices(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	internalDB := f.monitor(t, "prod-db-internal", store.MonitorStatusDown)

	f.publish(t, api.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Services) != 1 || page.Services[0].ID != api.ID {
		t.Fatalf("servisler = %+v, yalnızca API beklenmişti", page.Services)
	}

	// Private servis DOWN olmasına rağmen genel özet "çalışıyor": yayımlanmayan
	// bir servisin durumu ziyaretçinin gördüğü hiçbir sayıya girmiyor.
	if page.Overall != OverallOperational {
		t.Errorf("genel durum = %q, beklenen %q", page.Overall, OverallOperational)
	}

	assertNoLeak(t, page, internalDB.Name)
}

// --- Sızıntı: incident'lar --------------------------------------------------

func TestPublicHidesAutoIncidentOfPrivateMonitor(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	internalDB := f.monitor(t, "prod-db-internal", store.MonitorStatusDown)

	f.publish(t, api.ID)

	// 4.1'in otomatik incident'ı: BAŞLIĞI monitor adını içeriyor.
	f.autoIncident(t, internalDB)
	publicIncident := f.autoIncident(t, api)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Incidents) != 1 || page.Incidents[0].ID != publicIncident {
		t.Fatalf("kesintiler = %+v, yalnızca API'ninki beklenmişti", page.Incidents)
	}

	assertNoLeak(t, page, internalDB.Name)
}

func TestPublicManualIncidentShowsOnlyPublishedServices(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	internalDB := f.monitor(t, "prod-db-internal", store.MonitorStatusDown)

	f.publish(t, api.ID)

	// Manuel incident İKİ servisi kapsıyor; başlığı admin yazdı, yani
	// yayımlanabilir. Ama etkilenen servis listesinde private olan görünmemeli.
	f.manualIncident(t, "Veritabanı bakımı sırasında yavaşlama", api.ID, internalDB.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Incidents) != 1 {
		t.Fatalf("kesinti sayısı = %d, beklenen 1", len(page.Incidents))
	}

	services := page.Incidents[0].Services
	if len(services) != 1 || services[0].ID != api.ID {
		t.Fatalf("etkilenen servisler = %+v, yalnızca API beklenmişti", services)
	}

	assertNoLeak(t, page, internalDB.Name)
}

func TestPublicHidesIncidentWithoutPublishedService(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	internalDB := f.monitor(t, "prod-db-internal", store.MonitorStatusDown)

	f.publish(t, api.ID)
	f.manualIncident(t, "prod-db-internal üzerinde planlı çalışma", internalDB.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Incidents) != 0 {
		t.Fatalf("kesintiler = %+v, hiçbiri beklenmemişti", page.Incidents)
	}

	assertNoLeak(t, page, internalDB.Name)
}

func TestPublicIncidentUpdatesCarryNoAuthor(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	incidentID := f.manualIncident(t, "API yavaş yanıt veriyor", api.ID)

	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO incident_updates (incident_id, is_system, status, body)
		VALUES ($1, true, 'investigating', 'Otomatik tespit edildi.')`, incidentID); err != nil {
		t.Fatalf("timeline satırı yazılamadı: %v", err)
	}

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	updates := page.Incidents[0].Updates
	if len(updates) != 1 {
		t.Fatalf("timeline satır sayısı = %d, beklenen 1", len(updates))
	}
	if !updates[0].IsSystem {
		t.Error("sistem satırı isSystem taşımıyor")
	}
}

func TestPublicHidesOldResolvedIncidents(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	incidentID := f.manualIncident(t, "Çok eski bir kesinti", api.ID)

	// Pencerenin (RecentIncidentWindow) dışında kapanmış.
	if _, err := f.pool.Exec(t.Context(), `
		UPDATE incidents SET status = 'resolved',
		    started_at = now() - interval '100 days',
		    resolved_at = now() - interval '99 days'
		WHERE id = $1`, incidentID); err != nil {
		t.Fatalf("incident kapatılamadı: %v", err)
	}

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Incidents) != 0 {
		t.Fatalf("kesintiler = %+v, eski kayıt listede olmamalıydı", page.Incidents)
	}
}

// --- Sızıntı: bakım pencereleri ---------------------------------------------

func TestPublicMaintenanceOnlyForPublishedServices(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusDown)
	internalDB := f.monitor(t, "prod-db-internal", store.MonitorStatusUp)

	f.publish(t, api.ID)

	now := time.Now().UTC()
	// API AKTİF bakımda, private servis için ayrı bir pencere var.
	f.maintenanceWindow(t, "API yükseltmesi", now.Add(-time.Hour), now.Add(time.Hour), api.ID)
	f.maintenanceWindow(t, "prod-db-internal yükseltmesi",
		now.Add(2*time.Hour), now.Add(3*time.Hour), internalDB.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Maintenance) != 1 || page.Maintenance[0].Title != "API yükseltmesi" {
		t.Fatalf("bakım pencereleri = %+v, yalnızca API'ninki beklenmişti", page.Maintenance)
	}

	if len(page.Services) != 1 || !page.Services[0].InMaintenance {
		t.Fatalf("servis bakım işareti taşımıyor: %+v", page.Services)
	}

	// Servis DOWN ama AKTİF bakımda: 4.3'ün kuralı gereği kesinti ilan
	// edilmiyor (bkz. Summarize).
	if page.Overall != OverallMaintenance {
		t.Errorf("genel durum = %q, beklenen %q", page.Overall, OverallMaintenance)
	}

	assertNoLeak(t, page, internalDB.Name)
}

func TestPublicHidesPastMaintenance(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	now := time.Now().UTC()
	f.maintenanceWindow(t, "Geçmiş bakım", now.Add(-3*time.Hour), now.Add(-2*time.Hour), api.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Maintenance) != 0 {
		t.Fatalf("bakım pencereleri = %+v, geçmiş pencere listelenmemeliydi", page.Maintenance)
	}
}

// --- Admin tarafı -----------------------------------------------------------

func TestReplaceServicesReplacesWholeSelection(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	web := f.monitor(t, "Web", store.MonitorStatusUp)

	if _, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(api.ID, web.ID)); err != nil {
		t.Fatalf("servisler yayımlanamadı: %v", err)
	}

	settings, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(web.ID))
	if err != nil {
		t.Fatalf("seçim değiştirilemedi: %v", err)
	}

	if len(settings.Items) != 1 || settings.Items[0].MonitorID != web.ID {
		t.Fatalf("seçim = %+v, yalnızca Web beklenmişti", settings.Items)
	}

	// Boş liste de geçerli: sayfayı yayımdan kaldırmadan servisler geri
	// çekilebilmeli.
	settings, err = f.svc.ReplaceServices(t.Context(), f.orgID, ServicesInput{})
	if err != nil {
		t.Fatalf("seçim boşaltılamadı: %v", err)
	}
	if len(settings.Items) != 0 {
		t.Fatalf("seçim = %+v, boş beklenmişti", settings.Items)
	}
}

func TestReplaceServicesRollsBackOnForeignMonitor(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)

	if _, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(api.ID)); err != nil {
		t.Fatalf("servisler yayımlanamadı: %v", err)
	}

	// Başka bir organizasyonun monitor'ü. AYNI havuzda kuruluyor: ikinci bir
	// testdb.Pool çağrısı tabloları temizler ve bu testin kurulumunu silerdi.
	foreign := f.foreignMonitor(t)

	_, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(api.ID, foreign.ID))

	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("yabancı monitor kabul edildi: %v", err)
	}

	// ASIL ÖLÇÜLEN: işlem TÜMDEN geri alındı mı? Silme yapılıp ekleme
	// başarısız olsaydı sayfa servissiz kalırdı.
	settings, err := f.svc.Get(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("ayarlar okunamadı: %v", err)
	}
	if len(settings.Items) != 1 || settings.Items[0].MonitorID != api.ID {
		t.Fatalf("seçim = %+v, eski hâli korunmalıydı", settings.Items)
	}
}

func TestPublishToggleChangesVisibility(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	if _, err := f.svc.Public(t.Context(), f.page.Slug); err != nil {
		t.Fatalf("yayımlanmış sayfa okunamadı: %v", err)
	}

	unpublished := false

	settings, err := f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{IsPublished: &unpublished})
	if err != nil {
		t.Fatalf("yayımdan kaldırılamadı: %v", err)
	}
	if settings.Page.IsPublished {
		t.Error("sayfa hâlâ yayımda görünüyor")
	}

	// Yayımdan kaldırmak SEÇİMİ silmiyor: tekrar yayımlandığında aynı liste
	// geri geliyor.
	if len(settings.Items) != 1 {
		t.Fatalf("seçim = %+v, korunmalıydı", settings.Items)
	}

	if _, err := f.svc.Public(t.Context(), f.page.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("yayımdan kaldırılmış sayfa döndü: %v", err)
	}
}

func TestDeletingMonitorRemovesItFromPage(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	web := f.monitor(t, "Web", store.MonitorStatusUp)
	f.publish(t, api.ID, web.ID)

	if _, err := f.queries.DeleteMonitor(t.Context(), store.DeleteMonitorParams{
		ID:    web.ID,
		OrgID: f.orgID,
	}); err != nil {
		t.Fatalf("monitor silinemedi: %v", err)
	}

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	// Silinen servis yayımdan da düşüyor (cascade): "şu an ne durumda"
	// sorusuna cevabı olmayan bir satır status page'de duramaz.
	if len(page.Services) != 1 || page.Services[0].ID != api.ID {
		t.Fatalf("servisler = %+v, yalnızca API beklenmişti", page.Services)
	}
}

// assertNoLeak, yayımlanmamış bir servisin adının cevabın HİÇBİR alanında
// geçmediğini doğrular.
//
// Alan alan kontrol etmek yerine tüm cevabı taramak bilinçli: 5.3 ve 6.2 bu
// gövdeye yeni alanlar ekleyecek ve o alanların da aynı kurala tabi olduğunu
// hatırlatan şey bu test olacak.
func assertNoLeak(t *testing.T, page PublicPage, name string) {
	t.Helper()

	haystack := page.Title + " " + page.Slug

	for _, service := range page.Services {
		haystack += " " + service.Name
	}

	for _, incident := range page.Incidents {
		haystack += " " + incident.Title
		for _, service := range incident.Services {
			haystack += " " + service.Name
		}
		for _, update := range incident.Updates {
			haystack += " " + update.Body
		}
	}

	for _, window := range page.Maintenance {
		haystack += " " + window.Title
		if window.Description != nil {
			haystack += " " + *window.Description
		}
		for _, service := range window.Services {
			haystack += " " + service.Name
		}
	}

	if strings.Contains(haystack, name) {
		t.Fatalf("yayımlanmamış servisin adı (%q) public cevaba sızdı", name)
	}
}

// --- 90 günlük geçmiş (5.2) --------------------------------------------------

// rollup, bir monitor için günlük özet satırı yazar.
//
// Doğrudan INSERT: 3.4'ün toplulaştırması ham check satırlarından üretiyor ve
// bu testlerin ölçtüğü şey toplulaştırma değil, ÖZETİN GRAFİĞE ÇEVRİLMESİ.
func (f *fixture) rollup(
	t *testing.T, monitorID uuid.UUID, daysAgo int, total, success, degraded, failure, maintenance int32,
) {
	t.Helper()

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -daysAgo)

	if _, err := f.pool.Exec(t.Context(), `
		INSERT INTO uptime_rollup_daily (
		    monitor_id, bucket_date, total_checks, success_checks,
		    degraded_checks, failure_checks, maintenance_checks
		) VALUES ($1, $2::date, $3, $4, $5, $6, $7)`,
		monitorID, day, total, success, degraded, failure, maintenance); err != nil {
		t.Fatalf("günlük özet yazılamadı: %v", err)
	}
}

// dayAt, geçmiş dizisinde `daysAgo` gün önceki barı döndürür.
func dayAt(t *testing.T, history []UptimeDay, daysAgo int) UptimeDay {
	t.Helper()

	if len(history) != HistoryDays {
		t.Fatalf("geçmiş uzunluğu = %d, beklenen %d", len(history), HistoryDays)
	}

	return history[HistoryDays-1-daysAgo]
}

func TestHistoryAlwaysFillsEveryDay(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	// Tek bir günün özeti var; kalan 89 gün "veri yok" olarak DOLDURULMALI.
	f.rollup(t, api.ID, 3, 100, 100, 0, 0, 0)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	history := page.Services[0].History

	filled := dayAt(t, history, 3)
	if filled.State != DayStateUp {
		t.Errorf("verili gün durumu = %q, beklenen %q", filled.State, DayStateUp)
	}
	if filled.UptimePercent == nil || *filled.UptimePercent != 100 {
		t.Errorf("verili gün yüzdesi = %v", filled.UptimePercent)
	}

	// VERİ YOK GÜNÜ %100 DEĞİL, null (0.1'in bağlayıcı ayrımı).
	empty := dayAt(t, history, 4)
	if empty.State != DayStateNoData {
		t.Errorf("boş gün durumu = %q, beklenen %q", empty.State, DayStateNoData)
	}
	if empty.UptimePercent != nil {
		t.Errorf("boş gün yüzdesi = %v, null olmalıydı", *empty.UptimePercent)
	}

	// Diziler günlere göre sıralı ve son eleman BUGÜN.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if got := history[HistoryDays-1].Date; !got.Equal(today) {
		t.Errorf("son gün = %s, beklenen %s", got, today)
	}
}

func TestHistoryAppliesMaintenanceAwareFormula(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	// 4.3'ün dört parçası: 10 kontrolün 2'si bakımda, 6 başarılı, 1 yavaş,
	// 1 başarısız → (6 + 1) / (10 - 2) = %87.5.
	f.rollup(t, api.ID, 1, 10, 6, 1, 1, 2)
	// Günün TAMAMI bakımda: payda 0 → oran yok ama "veri yok" da değil.
	f.rollup(t, api.ID, 2, 5, 0, 0, 0, 5)
	// Hiç başarılı ölçüm yok.
	f.rollup(t, api.ID, 5, 4, 0, 0, 4, 0)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	history := page.Services[0].History

	partial := dayAt(t, history, 1)
	if partial.State != DayStatePartial {
		t.Errorf("kısmi gün durumu = %q", partial.State)
	}
	if partial.UptimePercent == nil || *partial.UptimePercent != 87.5 {
		t.Errorf("kısmi gün yüzdesi = %v, beklenen 87.5", partial.UptimePercent)
	}
	if !partial.HasMaintenance {
		t.Error("kısmi bakım günü işaretlenmedi")
	}

	full := dayAt(t, history, 2)
	if full.State != DayStateMaintenance {
		t.Errorf("tam bakım günü durumu = %q, beklenen %q", full.State, DayStateMaintenance)
	}
	if full.UptimePercent != nil {
		t.Errorf("tam bakım günü yüzdesi = %v, null olmalıydı", *full.UptimePercent)
	}

	down := dayAt(t, history, 5)
	if down.State != DayStateDown {
		t.Errorf("kesinti günü durumu = %q", down.State)
	}
	if down.UptimePercent == nil || *down.UptimePercent != 0 {
		t.Errorf("kesinti günü yüzdesi = %v, beklenen 0", down.UptimePercent)
	}
}

func TestHistoryAttachesIncidentsToTheirDays(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	incidentID := f.manualIncident(t, "Iki güne yayılan kesinti", api.ID)

	// 5 gün önce başlayıp 4 gün önce kapanmış: iki güne de düşmeli.
	if _, err := f.pool.Exec(t.Context(), `
		UPDATE incidents SET status = 'resolved',
		    started_at = (now() - interval '5 days'),
		    resolved_at = (now() - interval '4 days')
		WHERE id = $1`, incidentID); err != nil {
		t.Fatalf("incident güncellenemedi: %v", err)
	}

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	history := page.Services[0].History

	for _, daysAgo := range []int{5, 4} {
		day := dayAt(t, history, daysAgo)
		if len(day.Incidents) != 1 || day.Incidents[0].ID != incidentID {
			t.Errorf("%d gün önce kesinti bağlanmadı: %+v", daysAgo, day.Incidents)
		}
	}

	if day := dayAt(t, history, 3); len(day.Incidents) != 0 {
		t.Errorf("kesintisiz güne kesinti bağlandı: %+v", day.Incidents)
	}
}

func TestHistoryOfPrivateServiceNeverLeaks(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	internalDB := f.monitor(t, "prod-db-internal", store.MonitorStatusDown)

	f.publish(t, api.ID)

	// Private servisin hem özeti hem de adı başlığında geçen otomatik
	// incident'ı var — grafiğin 90 günlük penceresi 30 günlük incident
	// listesinden GENİŞ, yani ayrı bir sızıntı yüzeyi.
	f.rollup(t, internalDB.ID, 2, 10, 0, 0, 10, 0)
	f.rollup(t, api.ID, 2, 10, 10, 0, 0, 0)
	f.autoIncident(t, internalDB)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if len(page.Services) != 1 {
		t.Fatalf("servis sayısı = %d", len(page.Services))
	}

	for _, day := range page.Services[0].History {
		for _, incident := range day.Incidents {
			if strings.Contains(incident.Title, internalDB.Name) {
				t.Fatalf("private servisin adı grafik verisine sızdı: %q", incident.Title)
			}
		}
	}

	// Yayımlanan servisin kendi günü de doğru okunmuş olmalı (private satır
	// onun yerine geçmemiş).
	if day := dayAt(t, page.Services[0].History, 2); day.State != DayStateUp {
		t.Errorf("yayımlanan servisin günü = %q, beklenen %q", day.State, DayStateUp)
	}
}

// --- Özelleştirme ve gruplama (5.3) -----------------------------------------

func TestReplaceServicesStoresGrouping(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	web := f.monitor(t, "Web", store.MonitorStatusUp)
	other := f.monitor(t, "Zamanlanmış işler", store.MonitorStatusUp)

	settings, err := f.svc.ReplaceServices(t.Context(), f.orgID, ServicesInput{
		// SIRA kullanıcının kararı: "Web" alfabetik olarak önde ama listede
		// ikinci.
		Groups: []string{"API", "Web"},
		Items: []ItemInput{
			{MonitorID: web.ID, Group: "Web"},
			{MonitorID: api.ID, Group: "API"},
			{MonitorID: other.ID},
		},
	})
	if err != nil {
		t.Fatalf("servisler kaydedilemedi: %v", err)
	}

	if len(settings.Groups) != 2 || settings.Groups[0] != "API" || settings.Groups[1] != "Web" {
		t.Fatalf("gruplar = %v, sıra korunmalıydı", settings.Groups)
	}

	// Sıra: grup pozisyonu → grup adı → servis adı; gruplanmamış en sonda.
	want := []struct {
		name  string
		group *string
	}{
		{"API", ptr("API")},
		{"Web", ptr("Web")},
		{"Zamanlanmış işler", nil},
	}

	if len(settings.Items) != len(want) {
		t.Fatalf("servis sayısı = %d", len(settings.Items))
	}

	for i, expected := range want {
		got := settings.Items[i]
		if got.Name != expected.name {
			t.Errorf("%d. servis = %q, beklenen %q", i, got.Name, expected.name)
		}

		switch {
		case expected.group == nil && got.Group != nil:
			t.Errorf("%q gruplanmamış olmalıydı, grubu %q", got.Name, *got.Group)
		case expected.group != nil && (got.Group == nil || *got.Group != *expected.group):
			t.Errorf("%q grubu = %v, beklenen %q", got.Name, got.Group, *expected.group)
		}
	}
}

// Grubu SİLMEK servisi yayımdan DÜŞÜRMEMELİ (ON DELETE SET NULL): "gruplamayı
// yeniden düzenliyorum" hareketi sessizce bir servisi status page'den
// kaldırsaydı, 5.1'in sızıntı kaygısının ters yönü doğardı.
func TestReplaceServicesKeepsServiceWhenGroupRemoved(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)

	if _, err := f.svc.ReplaceServices(t.Context(), f.orgID, ServicesInput{
		Groups: []string{"API"},
		Items:  []ItemInput{{MonitorID: api.ID, Group: "API"}},
	}); err != nil {
		t.Fatalf("servisler kaydedilemedi: %v", err)
	}

	settings, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(api.ID))
	if err != nil {
		t.Fatalf("gruplama kaldırılamadı: %v", err)
	}

	if len(settings.Groups) != 0 {
		t.Errorf("gruplar = %v, boş beklenmişti", settings.Groups)
	}
	if len(settings.Items) != 1 || settings.Items[0].Group != nil {
		t.Fatalf("servis = %+v, gruplanmamış olarak durmalıydı", settings.Items)
	}
}

func TestUpdateSettingsAppliesAndClearsFields(t *testing.T) {
	f := newFixture(t)

	published := true
	settings, err := f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{
		IsPublished:   &published,
		Title:         ptr("  Acme durumu  "),
		SetTitle:      true,
		ThemeColor:    ptr("#AABBCC"),
		SetThemeColor: true,
		LogoURL:       ptr("https://cdn.example.com/logo.svg"),
		SetLogoURL:    true,
	})
	if err != nil {
		t.Fatalf("ayarlar kaydedilemedi: %v", err)
	}

	if settings.Page.Title == nil || *settings.Page.Title != "Acme durumu" {
		t.Errorf("başlık = %v", settings.Page.Title)
	}
	if settings.Page.ThemeColor == nil || *settings.Page.ThemeColor != "#aabbcc" {
		t.Errorf("renk = %v", settings.Page.ThemeColor)
	}

	// GÖNDERİLMEYEN ALAN DEĞİŞMİYOR (2.1'in PATCH kararı).
	settings, err = f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{
		Description:    ptr("Servislerimizin güncel durumu."),
		SetDescription: true,
	})
	if err != nil {
		t.Fatalf("açıklama kaydedilemedi: %v", err)
	}

	if settings.Page.Title == nil || !settings.Page.IsPublished {
		t.Errorf("dokunulmayan alanlar değişti: %+v", settings.Page)
	}

	// null GÖNDERMEK ALANI TEMİZLİYOR — "dokunma" ile aynı şey değil.
	settings, err = f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{SetTitle: true})
	if err != nil {
		t.Fatalf("başlık temizlenemedi: %v", err)
	}

	if settings.Page.Title != nil {
		t.Errorf("başlık = %v, temizlenmeliydi", *settings.Page.Title)
	}
	if settings.Page.Description == nil {
		t.Error("açıklama da silinmiş — yalnızca başlık gönderilmişti")
	}
}

func TestUpdateSettingsRejectsEmptyPatch(t *testing.T) {
	f := newFixture(t)

	// Boş gövde sessizce "hiçbir şey yapma" olmamalı.
	var validation *ValidationError
	if _, err := f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{}); !errors.As(err, &validation) {
		t.Fatalf("boş patch kabul edildi: %v", err)
	}
}

func TestPublicUsesCustomTitleAndTheme(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	// Varsayılan: özel başlık yoksa organizasyonun adı (5.1'in davranışı).
	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}
	if page.Title != "Acme A.Ş." {
		t.Errorf("başlık = %q, organizasyonun adı beklenmişti", page.Title)
	}

	if _, err := f.svc.UpdateSettings(t.Context(), f.orgID, SettingsPatch{
		Title:         ptr("Acme durumu"),
		SetTitle:      true,
		ThemeColor:    ptr("#ffffff"),
		SetThemeColor: true,
	}); err != nil {
		t.Fatalf("ayarlar kaydedilemedi: %v", err)
	}

	page, err = f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	if page.Title != "Acme durumu" {
		t.Errorf("başlık = %q", page.Title)
	}
	// Beyaz zeminin üzerine KOYU metin (bkz. theme.go).
	if page.ThemeTone != ToneDark {
		t.Errorf("ton = %q, beklenen %q", page.ThemeTone, ToneDark)
	}
}

func ptr[T any](value T) *T { return &value }

// --- 7.5: tek saat kaynağı ve tek servis okuması --------------------------

// CEVABIN "ŞİMDİ"Sİ VERİTABANINDAN GELİYOR (3.2'nin kararının okuma tarafı).
//
// Not: bu test bir REGRESYON KORUMASI, bir hata üretimi değil — makine ile
// veritabanı saati aynı olduğu sürece eski kod da geçerdi. Fail-first bir
// karşılığı yok çünkü tek yolu uygulamaya İKİNCİ bir saat kaynağı enjekte
// etmek olurdu; kaldırılan şey tam olarak o.
func TestPublicHistoryIsAnchoredToDatabaseClock(t *testing.T) {
	f := newFixture(t)
	api := f.monitor(t, "API", store.MonitorStatusUp)
	f.publish(t, api.ID)

	var dbToday time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT (now() AT TIME ZONE 'UTC')::date`).Scan(&dbToday); err != nil {
		t.Fatalf("veritabanı saati okunamadı: %v", err)
	}

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("sayfa okunamadı: %v", err)
	}

	history := page.Services[0].History
	if len(history) != HistoryDays {
		t.Fatalf("gün sayısı = %d, beklenen %d", len(history), HistoryDays)
	}

	last := history[HistoryDays-1].Date.UTC()
	if !last.Equal(dbToday.UTC()) {
		t.Errorf("son gün = %s, veritabanının bugünü %s", last, dbToday.UTC())
	}
}

// publicServices artık SAF: satırları çağıran okuyor, fonksiyon veritabanına
// gitmiyor. Kazancı iki yönlü — aynı istekte iki kez okunan sorgu teke indi ve
// "hangi servis şu an bakımda" kararı gerçek veri kurmadan sınanabiliyor.
func TestPublicServicesMarksOnlyActiveMaintenance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	aktif := uuid.New()
	yaklasan := uuid.New()
	gecmis := uuid.New()

	rows := []store.ListPublicServicesRow{
		{MonitorID: aktif, Name: "Aktif", CurrentStatus: store.MonitorStatusDown, StatusChangedAt: now},
		{MonitorID: yaklasan, Name: "Yaklaşan", CurrentStatus: store.MonitorStatusUp, StatusChangedAt: now},
		{MonitorID: gecmis, Name: "Geçmiş", CurrentStatus: store.MonitorStatusUp, StatusChangedAt: now},
	}

	windows := []PublicMaintenance{
		// Yarı açık aralık [starts, ends): şu an İÇİNDE.
		{StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
			Services: []PublicAffected{{ID: aktif}}},
		{StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour),
			Services: []PublicAffected{{ID: yaklasan}}},
		// Bitişi TAM ŞU AN: aralık yarı açık olduğu için artık bakımda değil.
		{StartsAt: now.Add(-2 * time.Hour), EndsAt: now,
			Services: []PublicAffected{{ID: gecmis}}},
	}

	histories := map[uuid.UUID][]UptimeDay{aktif: {{State: DayStateDown}}}

	services := publicServices(rows, windows, histories, now)

	if len(services) != 3 {
		t.Fatalf("servis sayısı = %d", len(services))
	}

	expected := map[uuid.UUID]bool{aktif: true, yaklasan: false, gecmis: false}
	for _, service := range services {
		if service.InMaintenance != expected[service.ID] {
			t.Errorf("%s inMaintenance = %v, beklenen %v",
				service.Name, service.InMaintenance, expected[service.ID])
		}
	}

	// Geçmiş, servisin KENDİ dizisinden geliyor.
	if len(services[0].History) != 1 {
		t.Errorf("aktif servisin geçmişi bağlanmadı: %#v", services[0].History)
	}
	if services[1].History != nil {
		t.Errorf("geçmişi olmayan servise dizi uyduruldu: %#v", services[1].History)
	}
}

// --- Bayatlık (8.6) ---------------------------------------------------------

// setLastChecked, monitor'ün son kontrol anını GEÇMİŞE çeker.
//
// Zamanı sahtelemek yerine satırı geriye almak: "şimdi" public okuma yolunda
// veritabanından (`server_now`) geliyor, yani testin saati değil VERİTABANININ
// saati geçerli. Bir clock arayüzü eklemek, üretimde hiç kullanılmayacak bir
// soyutlama olurdu.
func (f *fixture) setLastChecked(t *testing.T, monitorID uuid.UUID, ago time.Duration) {
	t.Helper()

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE monitors SET last_checked_at = now() - $2::interval WHERE id = $1`,
		monitorID, ago.String()); err != nil {
		t.Fatalf("son kontrol zamanı ayarlanamadı: %v", err)
	}
}

// WORKER ÖLÜNCE ZİYARETÇİ BUNU GÖRÜYOR — 8.6'nın kapattığı körlük.
//
// Monitor'ün `current_status`u `up` kalmaya devam ediyor (3.3 onu yalnızca
// yeni bir ölçümle değiştirir); bayatlık bunu "artık bilmiyoruz"a çeviriyor.
func TestPublicPageMarksStaleServiceAndDropsItFromSummary(t *testing.T) {
	f := newFixture(t)

	monitor := f.monitor(t, "Ölçümü Duran Servis", store.MonitorStatusUp)
	f.publish(t, monitor.ID)

	// Aralık 60 sn → eşik 180 sn. 10 dakika fazlasıyla bayat.
	f.setLastChecked(t, monitor.ID, 10*time.Minute)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("public sayfa okunamadı: %v", err)
	}

	if len(page.Services) != 1 {
		t.Fatalf("servis sayısı = %d", len(page.Services))
	}

	service := page.Services[0]
	if !service.Stale {
		t.Error("ölçümü durmuş servis bayat işaretlenmedi")
	}
	// SON BİLİNEN DURUM KORUNUYOR: bayatlık onu silmiyor, "güncel değil"
	// diyor. Arayüz rozeti buna göre çiziyor.
	if service.Status != store.MonitorStatusUp {
		t.Errorf("durum = %q, beklenen up (bayatlık son bilinen durumu silmiyor)", service.Status)
	}
	if service.LastCheckedAt == nil {
		t.Error("son kontrol zamanı dönmedi; arayüz 'N dakikadır ölçüm yok' yazamaz")
	}

	// ÖZET "çalışıyor" DEMİYOR.
	if page.Overall != OverallUnknown {
		t.Errorf("genel durum = %q, beklenen unknown", page.Overall)
	}
	if page.StaleServices != 1 {
		t.Errorf("bayat servis sayısı = %d, beklenen 1", page.StaleServices)
	}
}

// TAZE ÖLÇÜM BAYAT DEĞİL: yanlış alarm üretmediğinin kanıtı.
func TestPublicPageDoesNotMarkFreshServiceStale(t *testing.T) {
	f := newFixture(t)

	monitor := f.monitor(t, "Sağlam Servis", store.MonitorStatusUp)
	f.publish(t, monitor.ID)

	f.setLastChecked(t, monitor.ID, 30*time.Second)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("public sayfa okunamadı: %v", err)
	}

	if page.Services[0].Stale {
		t.Error("30 saniye önce kontrol edilmiş servis bayat sayıldı")
	}
	if page.Overall != OverallOperational {
		t.Errorf("genel durum = %q, beklenen operational", page.Overall)
	}
	if page.StaleServices != 0 {
		t.Errorf("bayat servis sayısı = %d, beklenen 0", page.StaleServices)
	}
}

// HİÇ KONTROL EDİLMEMİŞ SERVİS BAYAT DEĞİL (yanlış alarm kapısı).
//
// "Hiç ölçülmedi" ile "ölçüm durdu" farklı şeyler: yeni eklenmiş bir monitor
// zaten `pending` ve ziyaretçiye "ölçüm durdu" demek yanlış olurdu.
func TestPublicPageDoesNotMarkNeverCheckedServiceStale(t *testing.T) {
	f := newFixture(t)

	// `last_checked_at` hiç yazılmadı (yeni monitor).
	monitor := f.monitor(t, "Yeni Servis", store.MonitorStatusPending)
	f.publish(t, monitor.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("public sayfa okunamadı: %v", err)
	}

	if page.Services[0].Stale {
		t.Error("hiç kontrol edilmemiş servis bayat sayıldı")
	}
	if page.StaleServices != 0 {
		t.Errorf("bayat servis sayısı = %d, beklenen 0", page.StaleServices)
	}
}

// DURAKLATILMIŞ SERVİS BAYAT DEĞİL (yanlış alarm kapısı).
//
// Kullanıcı ölçmeyi bilerek durdurdu; kendi kararını arıza diye ilan etmek
// yanlış olurdu. Durumu zaten `paused` ve o da özete girmiyor (5.1).
func TestPublicPageDoesNotMarkPausedServiceStale(t *testing.T) {
	f := newFixture(t)

	monitor := f.monitor(t, "Duraklatılmış Servis", store.MonitorStatusPaused)
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE monitors SET is_active = false, next_check_at = NULL WHERE id = $1`,
		monitor.ID); err != nil {
		t.Fatalf("monitor duraklatılamadı: %v", err)
	}
	f.setLastChecked(t, monitor.ID, 6*time.Hour)

	f.publish(t, monitor.ID)

	page, err := f.svc.Public(t.Context(), f.page.Slug)
	if err != nil {
		t.Fatalf("public sayfa okunamadı: %v", err)
	}

	if page.Services[0].Stale {
		t.Error("duraklatılmış servis bayat sayıldı")
	}
	if page.StaleServices != 0 {
		t.Errorf("bayat servis sayısı = %d, beklenen 0", page.StaleServices)
	}
}
