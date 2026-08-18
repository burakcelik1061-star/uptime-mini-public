package checker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Testlerin tamamı YEREL: httptest sunucuları ve kapalı bir loopback portu
// kullanılıyor, dışarıya tek bir istek gitmiyor. Sınıflandırmanın ağ
// gerektiren dalları (DNS) saf fonksiyona doğrudan hata verilerek ölçülüyor
// (bkz. classify_test.go).

// defaultTarget, testlerin üzerine tek alan değiştirdiği taban hedef.
func defaultTarget(url string) Target {
	return Target{
		URL:               url,
		Method:            store.MonitorMethodGET,
		Timeout:           2 * time.Second,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
	}
}

func run(t *testing.T, target Target) Result {
	t.Helper()

	result, err := newTestChecker().Run(t.Context(), target)
	if err != nil {
		t.Fatalf("kontrol çalıştırılamadı: %v", err)
	}

	return result
}

func TestSuccessfulCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	result := run(t, defaultTarget(server.URL))

	if result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("outcome = %q, beklenen success (%s)", result.Outcome, result.ErrorMessage)
	}
	if result.HTTPStatus != 200 {
		t.Errorf("status = %d", result.HTTPStatus)
	}
	if result.ErrorType != "" {
		t.Errorf("hata tipi = %q, boş olmalıydı", result.ErrorType)
	}
	if result.ResponseTime <= 0 {
		t.Errorf("yanıt süresi ölçülmemiş: %v", result.ResponseTime)
	}
	if result.CheckedAt.IsZero() {
		t.Error("checkedAt boş")
	}
}

// Beklenen aralık yapılandırılabilir: kimlik isteyen bir uç için 401 SAĞLIKLI
// cevaptır (servis ayakta ve doğru davranıyor).
func TestExpectedStatusRangeIsHonoured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	target := defaultTarget(server.URL)
	target.ExpectedStatusMin = 401
	target.ExpectedStatusMax = 401

	if result := run(t, target); result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("outcome = %q, beklenen success", result.Outcome)
	}

	// Varsayılan aralıkta aynı cevap BAŞARISIZ.
	result := run(t, defaultTarget(server.URL))
	if result.Outcome != store.CheckOutcomeFailure {
		t.Fatalf("outcome = %q, beklenen failure", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeUnexpectedStatus {
		t.Errorf("hata tipi = %q", result.ErrorType)
	}
	// Status BİLİNİYOR: cevap alındı, beklenen aralıkta değildi.
	if result.HTTPStatus != 401 {
		t.Errorf("status = %d", result.HTTPStatus)
	}
	if !strings.Contains(result.ErrorMessage, "401") {
		t.Errorf("mesaj = %q", result.ErrorMessage)
	}
}

// "Çalışıyor ama yavaş": eşik aşıldığında outcome degraded, ama HATA YOK.
func TestSlowResponseIsDegraded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	target := defaultTarget(server.URL)
	target.DegradedAfter = 20 * time.Millisecond

	result := run(t, target)
	if result.Outcome != store.CheckOutcomeDegraded {
		t.Fatalf("outcome = %q, beklenen degraded (süre %v)", result.Outcome, result.ResponseTime)
	}
	if result.ErrorType != "" {
		t.Errorf("degraded bir hata değil, tip = %q", result.ErrorType)
	}
	if result.HTTPStatus != 200 {
		t.Errorf("status = %d", result.HTTPStatus)
	}
}

// Eşik yoksa (DegradedAfter sıfır) yavaşlık sinyali hiç üretilmiyor.
func TestNoDegradedThresholdMeansNoDegradedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
	}))
	defer server.Close()

	if result := run(t, defaultTarget(server.URL)); result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("outcome = %q, beklenen success", result.Outcome)
	}
}

// Beklenmeyen status HIZLI geldiyse bile başarısız — sıra önce status.
func TestUnexpectedStatusBeatsDegraded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	target := defaultTarget(server.URL)
	target.DegradedAfter = time.Nanosecond // her cevap "yavaş" sayılırdı

	result := run(t, target)
	if result.Outcome != store.CheckOutcomeFailure {
		t.Errorf("outcome = %q, beklenen failure", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeUnexpectedStatus {
		t.Errorf("hata tipi = %q", result.ErrorType)
	}
}

func TestTimeout(t *testing.T) {
	// Sunucu isteği askıya alıyor; kontrol kendi zaman aşımıyla dönmeli.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	target := defaultTarget(server.URL)
	target.Timeout = 100 * time.Millisecond

	started := time.Now()
	result := run(t, target)

	if result.Outcome != store.CheckOutcomeFailure {
		t.Fatalf("outcome = %q, beklenen failure", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeTimeout {
		t.Errorf("hata tipi = %q, beklenen timeout (%s)", result.ErrorType, result.ErrorMessage)
	}
	// Cevap alınamadı: status BİLİNMİYOR, uydurulmuyor.
	if result.HTTPStatus != 0 {
		t.Errorf("status = %d, 0 olmalıydı", result.HTTPStatus)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("zaman aşımı uygulanmadı, süre %v", elapsed)
	}
}

func TestConnectionRefused(t *testing.T) {
	// Dinlemeyi hemen bırakılan bir port: adres geçerli, kimse dinlemiyor.
	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dinleyici açılamadı: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	result := run(t, defaultTarget("http://"+address))

	if result.Outcome != store.CheckOutcomeFailure {
		t.Fatalf("outcome = %q, beklenen failure", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeConnectionRefused {
		t.Errorf("hata tipi = %q, beklenen connection_refused (%s)",
			result.ErrorType, result.ErrorMessage)
	}
}

// Kendi imzalı sertifika: istemci onu tanımıyor → TLS hatası.
func TestTLSError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := run(t, defaultTarget(server.URL))

	if result.Outcome != store.CheckOutcomeFailure {
		t.Fatalf("outcome = %q, beklenen failure", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeTls {
		t.Errorf("hata tipi = %q, beklenen tls (%s)", result.ErrorType, result.ErrorMessage)
	}
}

// https ile DÜZ http konuşan bir porta gitmek de TLS hatası
// (tls.RecordHeaderError) — sık yapılan yapılandırma hatası.
func TestPlainHTTPServerOnHTTPSURLIsTLSError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	httpsURL := "https://" + strings.TrimPrefix(server.URL, "http://")

	result := run(t, defaultTarget(httpsURL))
	if result.ErrorType != store.CheckErrorTypeTls {
		t.Errorf("hata tipi = %q, beklenen tls (%s)", result.ErrorType, result.ErrorMessage)
	}
}

// Yönlendirme İZLENİYOR ama sınırlı: döngü kontrolü sonsuza kadar meşgul
// etmesin.
func TestRedirectsAreFollowedUpToLimit(t *testing.T) {
	var server *httptest.Server
	hops := 0

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/son" {
			w.WriteHeader(http.StatusOK)
			return
		}

		hops++
		http.Redirect(w, r, server.URL+"/son", http.StatusFound)
	}))
	defer server.Close()

	if result := run(t, defaultTarget(server.URL)); result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("tek yönlendirme izlenmeliydi: %q (%s)", result.Outcome, result.ErrorMessage)
	}
	if hops != 1 {
		t.Errorf("yönlendirme sayısı = %d", hops)
	}
}

func TestRedirectLoopIsFailure(t *testing.T) {
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/tekrar", http.StatusFound)
	}))
	defer server.Close()

	result := run(t, defaultTarget(server.URL))
	if result.Outcome != store.CheckOutcomeFailure {
		t.Fatalf("outcome = %q, beklenen failure", result.Outcome)
	}
	// Şemada "çok fazla yönlendirme" diye bir tip yok; mesaj sebebi
	// söylüyor.
	if result.ErrorType != store.CheckErrorTypeUnknown {
		t.Errorf("hata tipi = %q", result.ErrorType)
	}
	if !strings.Contains(result.ErrorMessage, "yönlendirme") {
		t.Errorf("mesaj sebebi söylemiyor: %q", result.ErrorMessage)
	}
}

// Devasa gövde belleği doldurmamalı; kontrol yine de başarılı sayılmalı.
func TestBodyIsLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("a", 32<<10)
		for range 8 { // 256 KiB — sınırın dört katı
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	if result := run(t, defaultTarget(server.URL)); result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("outcome = %q (%s)", result.Outcome, result.ErrorMessage)
	}
}

// HEAD isteğinde gövde yok; kontrol yine çalışmalı.
func TestHeadMethod(t *testing.T) {
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	target := defaultTarget(server.URL)
	target.Method = store.MonitorMethodHEAD

	if result := run(t, target); result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("outcome = %q", result.Outcome)
	}
	if gotMethod != http.MethodHead {
		t.Errorf("metot = %q", gotMethod)
	}
}

// ÇAĞIRANIN İPTALİ SONUÇ DEĞİL: worker kapanırken yapılan iptal, sahte bir
// kesinti kaydına dönüşmemeli (3.2/4.1 için kritik).
func TestCallerCancellationIsNotAFailedResult(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	target := defaultTarget(server.URL)
	target.Timeout = 10 * time.Second // kendi zaman aşımımız çok uzak

	result, err := newTestChecker().Run(ctx, target)
	if !errors.Is(err, ErrCheckNotRun) {
		t.Fatalf("ErrCheckNotRun bekleniyordu, gelen: %v (sonuç %+v)", err, result)
	}
	if result.Outcome != "" {
		t.Errorf("iptalde sonuç üretilmemeli: %+v", result)
	}
}

// Bozuk URL (2.1'in doğrulaması atlanmış olsa bile) sessizce yutulmamalı.
func TestUnbuildableRequestIsAFailure(t *testing.T) {
	target := defaultTarget("http://%zz")

	result := run(t, target)
	if result.Outcome != store.CheckOutcomeFailure {
		t.Fatalf("outcome = %q", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeUnknown {
		t.Errorf("hata tipi = %q", result.ErrorType)
	}
}

// Her kontrol YENİ bir bağlantı kuruyor: ölçümler karşılaştırılabilir kalsın
// ve bayat bir bağlantı üzerinden "başarılı" sonuç üretilmesin.
func TestConnectionsAreNotReused(t *testing.T) {
	connections := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections[r.RemoteAddr] = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	checker := newTestChecker()
	for range 3 {
		if _, err := checker.Run(t.Context(), defaultTarget(server.URL)); err != nil {
			t.Fatalf("kontrol çalıştırılamadı: %v", err)
		}
	}

	if len(connections) != 3 {
		t.Errorf("farklı bağlantı sayısı = %d, beklenen 3", len(connections))
	}
}

func TestTargetOfReadsMonitor(t *testing.T) {
	threshold := int32(1500)
	monitor := store.Monitor{
		Url:                "https://example.com",
		Method:             store.MonitorMethodHEAD,
		TimeoutMs:          5000,
		ExpectedStatusMin:  200,
		ExpectedStatusMax:  399,
		DegradedResponseMs: &threshold,
	}

	target := TargetOf(monitor)

	if target.Timeout != 5*time.Second {
		t.Errorf("timeout = %v", target.Timeout)
	}
	if target.DegradedAfter != 1500*time.Millisecond {
		t.Errorf("degradedAfter = %v", target.DegradedAfter)
	}

	// Eşik yoksa sinyal kapalı.
	monitor.DegradedResponseMs = nil
	if got := TargetOf(monitor).DegradedAfter; got != 0 {
		t.Errorf("degradedAfter = %v, 0 olmalıydı", got)
	}
}

// newTestChecker, SSRF kapısı AÇIK bir checker döner.
//
// Testlerin hedefi `httptest` sunucuları, yani loopback (7.4'ün koruması
// tam olarak orayı reddediyor). Kapının kendisi internal/safedial'da
// ölçülüyor; buradaki testlerin konusu kontrolün DAVRANIŞI.
func newTestChecker() *Checker {
	return New(safedial.Policy{AllowPrivate: true})
}

// SSRF KORUMASI REDDEDİNCE SONUÇ `blocked_url` OLUYOR (7.4).
//
// Bu, 2.2'den beri şemada bekleyen hata tipinin ilk gerçek kullanıcısı.
// Sessizce "unknown" ya da "connection_refused" olsaydı kullanıcı hedefinin
// neden hiç kontrol edilmediğini anlayamaz, biz de korumanın çalışıp
// çalışmadığını loglardan ayırt edemezdik.
func TestBlockedTargetProducesBlockedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Üretim politikası: iç ağa izin yok. Hedef loopback'te.
	result, err := New(safedial.Policy{}).Run(t.Context(), defaultTarget(server.URL))
	if err != nil {
		t.Fatalf("Run hata döndürdü: %v", err)
	}

	if result.Outcome != store.CheckOutcomeFailure {
		t.Errorf("sonuç = %q, beklenen failure", result.Outcome)
	}
	if result.ErrorType != store.CheckErrorTypeBlockedUrl {
		t.Errorf("hata tipi = %q, beklenen blocked_url", result.ErrorType)
	}
	if result.HTTPStatus != 0 {
		t.Errorf("status = %d, beklenen 0 — istek hiç gönderilmemeliydi", result.HTTPStatus)
	}
	// Ölçüm YAZILIYOR ama "kontrol yapılmadı" DEĞİL: bu bir sonuç
	// (2.2'nin ayrımı) — monitor sessizce hiç kontrol edilmemiş görünmesin.
	if result.CheckedAt.IsZero() {
		t.Error("checkedAt boş")
	}
}

// Kapı açıkken aynı hedef normal bir kontrol sonucu üretiyor: koruma
// "her şeyi reddet"e dönüşmedi.
func TestAllowedPrivateTargetIsCheckedNormally(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result, err := newTestChecker().Run(t.Context(), defaultTarget(server.URL))
	if err != nil {
		t.Fatalf("Run hata döndürdü: %v", err)
	}

	if result.Outcome != store.CheckOutcomeSuccess {
		t.Errorf("sonuç = %q, beklenen success", result.Outcome)
	}
	if result.ErrorType == store.CheckErrorTypeBlockedUrl {
		t.Error("kapı açıkken blocked_url üretildi")
	}
}
