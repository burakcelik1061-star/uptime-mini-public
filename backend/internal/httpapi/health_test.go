package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

// newTestRouter, sağlık uçlarının testleri için GERÇEK router'ı kurar —
// auth bağımlılıkları da dahil. Testin kendi "yarım uygulama" kopyasını
// tutması, prod'da olan bir middleware'in testlerde eksik kalmasına yol
// açıyordu (bkz. router.go'daki not).
func newTestRouter(t *testing.T, startedAt time.Time, db Pinger) http.Handler {
	t.Helper()

	deps := newTestDeps(t)
	deps.StartedAt = startedAt
	deps.DB = db

	return NewRouter(deps)
}

func get(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))

	return rec
}

func TestHealthReturnsOK(t *testing.T) {
	router := newTestRouter(t, time.Now().Add(-90*time.Second), stubPinger{})

	rec := get(t, router, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen %d", rec.Code, http.StatusOK)
	}

	var body HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}

	if body.Status != "ok" || body.Service != "api" {
		t.Errorf("gövde = %+v, beklenen status=ok service=api", body)
	}
	if body.UptimeSeconds < 89 {
		t.Errorf("uptimeSeconds = %d, ~90 bekleniyordu", body.UptimeSeconds)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
}

// Liveness'ın veritabanına BAKMAMASI bilinçli bir karar (bkz. health.go);
// bu test o kararı sabitliyor: veritabanı düştüğünde /health hâlâ 200 döner,
// yoksa orkestratör sağlıklı süreci gereksiz yere yeniden başlatır.
func TestHealthStaysOKWhenDatabaseIsDown(t *testing.T) {
	router := newTestRouter(t, time.Now(), stubPinger{err: errors.New("bağlantı yok")})

	if rec := get(t, router, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen %d", rec.Code, http.StatusOK)
	}
}

func TestReadyReportsDatabaseUp(t *testing.T) {
	router := newTestRouter(t, time.Now(), stubPinger{})

	rec := get(t, router, "/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen %d", rec.Code, http.StatusOK)
	}

	var body ReadyResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}
	if body.Status != "ready" || body.Checks["database"] != "ok" {
		t.Errorf("gövde = %+v", body)
	}
}

func TestReadyReportsDatabaseDown(t *testing.T) {
	router := newTestRouter(t, time.Now(), stubPinger{err: errors.New("bağlantı yok")})

	rec := get(t, router, "/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, beklenen %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body ReadyResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}
	if body.Status != "degraded" || body.Checks["database"] != "unreachable" {
		t.Errorf("gövde = %+v", body)
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	router := newTestRouter(t, time.Now(), stubPinger{})

	if rec := get(t, router, "/yok-boyle-bir-yol"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, beklenen %d", rec.Code, http.StatusNotFound)
	}
}

// Middleware zincirinin gerçekten bağlı olduğunu ölçer: Recoverer olmasaydı
// panikleyen bir handler test sürecini düşürürdü.
func TestRecovererKeepsProcessAlive(t *testing.T) {
	router := newTestRouter(t, time.Now(), stubPinger{})

	mux, ok := router.(interface {
		Get(pattern string, h http.HandlerFunc)
		ServeHTTP(http.ResponseWriter, *http.Request)
	})
	if !ok {
		t.Fatal("router beklenen arayüzü karşılamıyor")
	}

	mux.Get("/panik", func(http.ResponseWriter, *http.Request) {
		panic("test paniği")
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/panik", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen %d", rec.Code, http.StatusInternalServerError)
	}
}
