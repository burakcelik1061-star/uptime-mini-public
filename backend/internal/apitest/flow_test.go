package apitest

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/scheduler"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// newTargetServer, monitor'lerin göstereceği KONTROLLÜ bir hedef döner.
//
// Dış dünyaya hiç çıkılmıyor: kontrol gerçekten HTTP üzerinden yapılıyor ama
// hedefi test kendisi ayağa kaldırıyor (7.2'nin E2E'sinde de aynı kural).
func newTargetServer(t *testing.T, status int) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server.URL
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runScheduler, worker'ın bir turunu ÇALIŞTIRIR.
//
// Süreç ayağa kaldırılmıyor, `RunDue` doğrudan çağrılıyor: buradaki soru
// worker'ın süreç çatısı (3.1, kendi testleri var) değil, kontrol → sonuç →
// durum makinesi → kesinti zincirinin sonucunun api'nin ucundan GÖRÜNMESİ.
func runScheduler(t *testing.T, h *Harness) {
	t.Helper()

	svc := scheduler.NewService(
		h.Queries,
		checker.New(testDialPolicy),
		results.NewService(h.Queries, discardLogger()),
		incidents.NewService(h.Pool, h.Queries, discardLogger()),
		// Yayıncının public kapısı da GERÇEK: yayımlanmamış bir servisin
		// değişikliğinin public kanala gövdesiz bir sinyal olarak bile
		// çıkmaması 6.2'nin kararı ve o kapı burada devrede.
		realtime.NewBroadcaster(
			realtime.NewNotifier(h.Queries, discardLogger()),
			statuspage.NewService(h.Pool, h.Queries, discardLogger()),
			discardLogger(),
		),
		discardLogger(),
		scheduler.Options{MaxConcurrentChecks: 4},
	)

	if _, err := svc.RunDue(t.Context()); err != nil {
		t.Fatalf("scheduler turu başarısız: %v", err)
	}
}

// Kayıt olmaktan public status sayfasına kadar TEK bir istemci akışı.
//
// Her adım gerçek bir HTTP isteği: sözleşme (durum kodları, gövde alanları,
// yönlendirme) yolun tamamında ölçülüyor.
func TestJourneyFromRegistrationToPublicPage(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)

	user := h.Register()
	org := h.CreateOrg(user)
	public := h.CreateMonitor(user, org, "Yayımlanmış API", target)
	private := h.CreateMonitor(user, org, "Gizli Iç Servis", target)

	slug := h.StatusPageSlug(user, org)

	// YAYIMLANMADAN ÖNCE: sayfa yok. "Yayımlanmamış ≡ olmayan slug" kuralı
	// (5.1) public UÇTA da geçerli olmalı.
	before := h.Do(Request{Method: http.MethodGet, Path: "/v1/public/status/" + slug})
	if before.Status != http.StatusNotFound {
		t.Fatalf("yayımlanmamış sayfa durumu = %d, beklenen 404", before.Status)
	}

	h.PublishStatusPage(user, org, public.ID)

	resp := h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   "/v1/public/status/" + slug,
	}), http.StatusOK)

	var page struct {
		Overall  string `json:"overall"`
		Services []struct {
			Name string `json:"name"`
		} `json:"services"`
	}
	h.JSON(resp, &page)

	if len(page.Services) != 1 || page.Services[0].Name != public.Name {
		t.Fatalf("yayımlanan servisler = %+v, beklenen tek servis %q",
			page.Services, public.Name)
	}

	// SIZINTI: private monitor'ün adı cevabın HİÇBİR yerinde geçmemeli —
	// alan alan değil, gövdenin tamamı taranıyor (5.1'in disiplini).
	if BodyContains(resp.Body, private.Name) {
		t.Errorf("public cevap yayımlanmamış servisin adını içeriyor: %s", resp.Body)
	}

	// PUBLIC UÇ OTURUM İSTEMİYOR: token'sız çağrı da aynı cevabı vermeli
	// (yukarıdaki istekler zaten token'sızdı) ve token GÖNDERİLSE bile
	// cevap değişmemeli — public yüzey oturum ağacının dışında (5.1).
	withToken := h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   "/v1/public/status/" + slug,
		Token:  user.Token,
	}), http.StatusOK)

	if len(withToken.Body) == 0 {
		t.Error("token'lı public istek boş gövde döndü")
	}
}

// Worker'ın açtığı OTOMATİK kesinti, public sayfada görünüyor.
//
// Bu zincir bu adıma kadar parça parça ölçülüyordu: kontrol→durum→kesinti
// internal/scheduler'da (gerçek DB), sızıntı kuralı internal/statuspage'de.
// Aralarındaki bağ — worker'ın yazdığı kesintinin ziyaretçinin gördüğü
// sayfaya ÇIKMASI — ilk kez burada uçtan uca ölçülüyor.
func TestAutomaticOutageBecomesVisibleOnPublicPage(t *testing.T) {
	h := New(t)

	failing := newTargetServer(t, http.StatusInternalServerError)

	user := h.Register()
	org := h.CreateOrg(user)

	// `failureThreshold: 1` — kesinti İLK hatada açılsın. Eşik monitor
	// bazında (3.3, alt sınır 1); testin gerçek zamanı beklemesi gerekmiyor.
	monitor := h.CreateMonitorWith(user, org, fmt.Sprintf(
		`{"name":"Düşen Servis","url":%q,"checkIntervalSeconds":30,"timeoutMs":5000,"failureThreshold":1}`,
		failing))

	slug := h.StatusPageSlug(user, org)
	h.PublishStatusPage(user, org, monitor.ID)

	// Kesinti YOKKEN: sayfa "çalışıyor" diyor ve kesinti listesi boş.
	var page struct {
		Overall   string `json:"overall"`
		Incidents []struct {
			Title      string  `json:"title"`
			Status     string  `json:"status"`
			ResolvedAt *string `json:"resolvedAt"`
		} `json:"incidents"`
		Services []struct {
			Status string `json:"status"`
		} `json:"services"`
	}

	h.JSON(h.Expect(h.Do(Request{
		Method: http.MethodGet, Path: "/v1/public/status/" + slug,
	}), http.StatusOK), &page)

	if len(page.Incidents) != 0 {
		t.Fatalf("kontrol öncesi kesinti listesi dolu: %+v", page.Incidents)
	}

	runScheduler(t, h)

	h.JSON(h.Expect(h.Do(Request{
		Method: http.MethodGet, Path: "/v1/public/status/" + slug,
	}), http.StatusOK), &page)

	if len(page.Incidents) != 1 {
		t.Fatalf("kesinti sayısı = %d, beklenen 1", len(page.Incidents))
	}
	if page.Incidents[0].ResolvedAt != nil {
		t.Error("kesinti kapalı görünüyor — otomatik açılan kayıt açık olmalı")
	}
	if page.Incidents[0].Status == "resolved" {
		t.Errorf("kesinti durumu = %q, açık bir kesinti beklenirdi", page.Incidents[0].Status)
	}
	if len(page.Services) != 1 || page.Services[0].Status != "down" {
		t.Errorf("servis durumu = %+v, beklenen down", page.Services)
	}
	if page.Overall == "operational" {
		t.Error("genel durum hâlâ 'operational' — düşen servis sayıma girmemiş")
	}

	// Pano tarafı da aynı şeyi söylemeli: iki yüzün ayrışması, 3.3/4.1'in
	// "durum tek yerden geçer" kuralının delinmesi demek.
	var listed struct {
		Incidents []struct {
			IsAutoCreated bool `json:"isAutoCreated"`
		} `json:"incidents"`
	}
	h.JSON(h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/incidents", org.ID),
		Token:  user.Token,
	}), http.StatusOK), &listed)

	if len(listed.Incidents) != 1 || !listed.Incidents[0].IsAutoCreated {
		t.Errorf("pano kesinti listesi = %+v, beklenen tek OTOMATİK kesinti", listed.Incidents)
	}
}

// Yayımlanmamış bir servisin kesintisi public sayfaya ÇIKMIYOR — otomatik
// kesintinin BAŞLIĞI monitor adını taşıdığı için başlık da bir sızıntı
// yüzeyi (5.1). Kural SQL'de yazılı; burada uçtan uca doğrulanıyor.
func TestOutageOfUnpublishedMonitorNeverReachesPublicPage(t *testing.T) {
	h := New(t)

	healthy := newTargetServer(t, http.StatusOK)
	failing := newTargetServer(t, http.StatusInternalServerError)

	user := h.Register()
	org := h.CreateOrg(user)

	shown := h.CreateMonitor(user, org, "Yayımlanmış Servis", healthy)
	hidden := h.CreateMonitorWith(user, org, fmt.Sprintf(
		`{"name":"Gizli Kuyruk Islemcisi","url":%q,"checkIntervalSeconds":30,"timeoutMs":5000,"failureThreshold":1}`,
		failing))

	slug := h.StatusPageSlug(user, org)
	h.PublishStatusPage(user, org, shown.ID)

	runScheduler(t, h)

	resp := h.Expect(h.Do(Request{
		Method: http.MethodGet, Path: "/v1/public/status/" + slug,
	}), http.StatusOK)

	if BodyContains(resp.Body, hidden.Name) {
		t.Errorf("public cevap yayımlanmamış servisin adını içeriyor: %s", resp.Body)
	}

	var page struct {
		Incidents []struct {
			Title string `json:"title"`
		} `json:"incidents"`
	}
	h.JSON(resp, &page)

	if len(page.Incidents) != 0 {
		t.Errorf("yayımlanmamış servisin kesintisi public sayfada: %+v", page.Incidents)
	}

	// Kesinti GERÇEKTEN açılmış olmalı — yoksa test "kesinti yok" diye
	// yanlış sebeple geçerdi.
	var listed struct {
		Incidents []struct {
			Title string `json:"title"`
		} `json:"incidents"`
	}
	h.JSON(h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/incidents", org.ID),
		Token:  user.Token,
	}), http.StatusOK), &listed)

	if len(listed.Incidents) != 1 {
		t.Fatalf("panoda kesinti sayısı = %d, beklenen 1 (test yanlış sebeple geçiyor olabilir)",
			len(listed.Incidents))
	}
}

// Manuel kontrol ucu GERÇEKTEN dışarıya istek atıyor ve sonucu saklanmıyor
// ama sayılıyor (2.2/2.3). Zincirin api ucundan görünen hâli.
func TestManualCheckReachesTheTargetAndIsMarked(t *testing.T) {
	h := New(t)

	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	user := h.Register()
	org := h.CreateOrg(user)
	monitor := h.CreateMonitor(user, org, "Elle Kontrol", target.URL)

	resp := h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/v1/orgs/%s/monitors/%s/check", org.ID, monitor.ID),
		Token:  user.Token,
	}), http.StatusOK)

	if hits != 1 {
		t.Fatalf("hedefe gelen istek sayısı = %d, beklenen 1", hits)
	}

	var payload struct {
		Check struct {
			Outcome string `json:"outcome"`
			Status  *int32 `json:"httpStatus"`
		} `json:"check"`
	}
	h.JSON(resp, &payload)

	if payload.Check.Outcome != "success" {
		t.Errorf("sonuç = %q, beklenen success", payload.Check.Outcome)
	}

	// MANUEL işaretlenmiş olmalı: uptime hesabına girmemesinin tek dayanağı
	// satırdaki bu ayrım (2.3).
	summary := h.summarize(user, org, monitor, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if summary.ManualChecks != 1 {
		t.Errorf("manuel kontrol sayısı = %d, beklenen 1", summary.ManualChecks)
	}
	if summary.ScheduledChecks != 0 {
		t.Errorf("zamanlanmış kontrol sayısı = %d, beklenen 0", summary.ScheduledChecks)
	}
	if summary.UptimePercent != nil {
		t.Errorf("uptime = %v, beklenen null (hiç zamanlanmış kontrol yok)", *summary.UptimePercent)
	}
}

// summaryResponse, /summary ucunun cevabı (2.4'ün sözleşmesi).
type summaryResponse struct {
	UptimePercent     *float64 `json:"uptimePercent"`
	ScheduledChecks   int64    `json:"scheduledChecks"`
	SuccessChecks     int64    `json:"successChecks"`
	DegradedChecks    int64    `json:"degradedChecks"`
	FailureChecks     int64    `json:"failureChecks"`
	MaintenanceChecks int64    `json:"maintenanceChecks"`
	ManualChecks      int64    `json:"manualChecks"`
}

func (h *Harness) summarize(user User, org Org, monitor Monitor, from, to time.Time) summaryResponse {
	h.t.Helper()

	resp := h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path: fmt.Sprintf("/v1/orgs/%s/monitors/%s/summary?from=%s&to=%s",
			org.ID, monitor.ID,
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)),
		Token: user.Token,
	}), http.StatusOK)

	var payload struct {
		Summary summaryResponse `json:"summary"`
	}
	h.JSON(resp, &payload)

	return payload.Summary
}

// 0.1'in uptime tanımı API SÖZLEŞMESİNDEN de aynı cevabı veriyor:
// `(success + degraded) / (total - maintenance)`.
//
// Formülün kendisi internal/results'ta ölçülüyor; buradaki soru arayüzün
// GÖRDÜĞÜ değerin aynı olması — sunucudan istemciye giden yolda bir yuvarlama
// ya da alan karışması olsaydı pano yanlış bir yüzde çizerdi.
func TestUptimeContractCountsDegradedAsUpAndExcludesMaintenance(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)

	user := h.Register()
	org := h.CreateOrg(user)
	monitor := h.CreateMonitor(user, org, "Karışık Ölçüm", target)

	from := time.Now().Add(-2 * time.Hour)
	to := time.Now().Add(time.Hour)

	recorder := results.NewService(h.Queries, discardLogger())
	record := func(outcome store.CheckOutcome, at time.Time) {
		t.Helper()

		result := checker.Result{
			Outcome:      outcome,
			HTTPStatus:   200,
			ResponseTime: 120 * time.Millisecond,
			CheckedAt:    at,
		}
		if outcome == store.CheckOutcomeFailure {
			result.HTTPStatus = 500
			result.ErrorType = store.CheckErrorTypeUnexpectedStatus
			result.ErrorMessage = "beklenmeyen durum"
		}

		if _, err := recorder.Record(
			t.Context(), monitor.ID, store.CheckTriggerScheduled, result); err != nil {
			t.Fatalf("sonuç yazılamadı: %v", err)
		}
	}

	base := time.Now().Add(-time.Hour)
	// 6 başarı + 2 degraded + 2 hata → (6+2)/10 = %80.
	for i := range 6 {
		record(store.CheckOutcomeSuccess, base.Add(time.Duration(i)*time.Minute))
	}
	for i := range 2 {
		record(store.CheckOutcomeDegraded, base.Add(time.Duration(6+i)*time.Minute))
	}
	for i := range 2 {
		record(store.CheckOutcomeFailure, base.Add(time.Duration(8+i)*time.Minute))
	}

	summary := h.summarize(user, org, monitor, from, to)

	if summary.ScheduledChecks != 10 {
		t.Fatalf("zamanlanmış kontrol = %d, beklenen 10", summary.ScheduledChecks)
	}
	if summary.UptimePercent == nil {
		t.Fatal("uptime null döndü")
	}
	if got := *summary.UptimePercent; got < 79.99 || got > 80.01 {
		t.Errorf("uptime = %v, beklenen 80 (degraded UP sayılır)", got)
	}
	if summary.DegradedChecks != 2 || summary.FailureChecks != 2 || summary.SuccessChecks != 6 {
		t.Errorf("bölüşüm = %+v, beklenen 6/2/2", summary)
	}
}
