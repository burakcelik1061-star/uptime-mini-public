package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// testWebOrigin, test router'ının izin verdiği tek origin.
const testWebOrigin = "http://localhost:3100"

func origin(value string) map[string]string {
	return map[string]string{"Origin": value}
}

func preflight(origin, method string) map[string]string {
	return map[string]string{
		"Origin":                        origin,
		"Access-Control-Request-Method": method,
	}
}

func TestPreflightFromAllowedOrigin(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	rec := do(t, router, http.MethodOptions, "/v1/orgs", "", preflight(testWebOrigin, http.MethodPost))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, beklenen 204 (%s)", rec.Code, rec.Body.String())
	}

	header := rec.Header()
	if got := header.Get("Access-Control-Allow-Origin"); got != testWebOrigin {
		t.Errorf("Allow-Origin = %q, beklenen %q", got, testWebOrigin)
	}
	// Kimlik bilgisi (refresh cookie) taşıyabilmesi için şart.
	if got := header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q", got)
	}
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		if !strings.Contains(header.Get("Access-Control-Allow-Methods"), method) {
			t.Errorf("Allow-Methods %q içermiyor: %q", method, header.Get("Access-Control-Allow-Methods"))
		}
	}
	if !strings.Contains(header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("Allow-Headers = %q", header.Get("Access-Control-Allow-Headers"))
	}
	if header.Get("Access-Control-Max-Age") == "" {
		t.Error("Max-Age yazılmamış")
	}
	// Vary olmadan bir ara önbellek, bir origin'in cevabını başkasına
	// servis edebilirdi.
	if !strings.Contains(strings.Join(header.Values("Vary"), ","), "Origin") {
		t.Errorf("Vary = %v", header.Values("Vary"))
	}
}

// İzin verilmeyen origin'in preflight'ı 403 alıyor ve CORS başlığı GÖRMÜYOR.
func TestPreflightFromDisallowedOrigin(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	rec := do(t, router, http.MethodOptions, "/v1/orgs", "", preflight("https://kotu.example", http.MethodPost))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, beklenen 403 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin yazılmamalıydı: %q", got)
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeForbidden {
		t.Errorf("code = %q", code)
	}
}

// Wildcard HİÇBİR ZAMAN yazılmıyor: `*` ile kimlik bilgisi taşıyan istek
// tarayıcı tarafından zaten reddedilir, ama yazılabilseydi cevaplar herkese
// açılırdı.
func TestCORSNeverUsesWildcard(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	for _, headers := range []map[string]string{
		origin(testWebOrigin),
		preflight(testWebOrigin, http.MethodGet),
		origin("https://kotu.example"),
	} {
		rec := do(t, router, http.MethodGet, "/health", "", headers)
		if rec.Header().Get("Access-Control-Allow-Origin") == "*" {
			t.Error("Allow-Origin `*` olmamalı")
		}
	}
}

func TestSimpleRequestFromAllowedOrigin(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	rec := do(t, router, http.MethodGet, "/health", "", origin(testWebOrigin))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testWebOrigin {
		t.Errorf("Allow-Origin = %q", got)
	}
}

// İzin verilmeyen origin'in isteği İŞLENİYOR ama cevabı CORS başlığı almıyor:
// tarayıcı cevabı JavaScript'e vermiyor. Sunucu tarafındaki asıl koruma
// kimlik doğrulaması (bkz. cors.go).
func TestSimpleRequestFromDisallowedOriginGetsNoCORSHeaders(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	rec := do(t, router, http.MethodGet, "/health", "", origin("https://kotu.example"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin yazılmamalıydı: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials yazılmamalıydı: %q", got)
	}
}

// Origin başlığı olmayan istek (curl, compose healthcheck) CORS'un konusu
// değil — başlık eklenmiyor.
func TestRequestWithoutOriginIsUntouched(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	rec := do(t, router, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || len(rec.Header().Values("Vary")) != 0 {
		t.Errorf("başlıklar eklenmiş: %v", rec.Header())
	}
}

// Access-Control-Request-Method taşımayan bir OPTIONS preflight DEĞİLDİR;
// normal istek gibi router'a düşmeli (burada: bu uçta OPTIONS tanımlı değil).
func TestOptionsWithoutPreflightHeaderIsNotShortCircuited(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	rec := do(t, router, http.MethodOptions, "/health", "", origin(testWebOrigin))
	if rec.Code == http.StatusNoContent {
		t.Fatalf("preflight sayılmamalıydı (status = %d)", rec.Code)
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, beklenen 405 (%s)", rec.Code, rec.Body.String())
	}
}

// Yapılandırmada sondaki eğik çizgi yaygın bir yazım hatası; Origin başlığı
// hiçbir zaman onu taşımıyor.
func TestConfiguredOriginTrailingSlashIsTolerated(t *testing.T) {
	deps := newTestDeps(t)
	deps.WebOrigins = []string{"http://localhost:3100/"}
	router := NewRouter(deps)

	rec := do(t, router, http.MethodGet, "/health", "", origin("http://localhost:3100"))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3100" {
		t.Errorf("Allow-Origin = %q", got)
	}
}

// WEB_ORIGINS boşsa hiçbir origin'e izin verilmiyor (açık taraf değil, kapalı
// taraf); kurulum sırasında uyarı loglanıyor.
func TestEmptyWebOriginsAllowsNothing(t *testing.T) {
	deps := newTestDeps(t)
	deps.WebOrigins = nil
	router := NewRouter(deps)

	rec := do(t, router, http.MethodGet, "/health", "", origin(testWebOrigin))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, boş olmalıydı", got)
	}
}

// TARAYICIDA YAKALANAN HATANIN TESTİ (5.1): izin verilen metot listesi,
// router'ın GERÇEKTEN kaydettiği metotların hepsini kapsamalı.
//
// `PUT` eklendiğinde bu liste güncellenmemişti ve sonuç sessizdi: preflight
// cevabı metodu saymadığı için tarayıcı asıl isteği HİÇ göndermiyor, sunucu
// logunda iz kalmıyor, arayüzde ise ağ hatasından ayırt edilemeyen bir mesaj
// çıkıyordu. Sabit bir liste yazmak yerine router'ın kendisini gezmek, aynı
// hatanın yeni bir metotla tekrarlanmasını da kapatıyor.
func TestCORSAllowedMethodsCoverEveryRoute(t *testing.T) {
	router := NewRouter(newTestDeps(t))

	routes, ok := router.(chi.Routes)
	if !ok {
		t.Fatal("router chi.Routes değil; bu test yolları gezemiyor")
	}

	allowed := make(map[string]struct{})
	for _, method := range strings.Split(corsAllowedMethods, ",") {
		allowed[strings.TrimSpace(method)] = struct{}{}
	}

	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if _, listed := allowed[method]; !listed {
			t.Errorf("%s %s: metot corsAllowedMethods listesinde yok", method, route)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("rotalar gezilemedi: %v", err)
	}
}
