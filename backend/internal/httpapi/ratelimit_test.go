package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// fixedClock, sınırlayıcının saatini testin eline verir: gerçek zamanı
// beklemek testi hem yavaş hem kırılgan yapardı.
type fixedClock struct{ at time.Time }

func (c *fixedClock) now() time.Time { return c.at }

func (c *fixedClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newTestLimiter(burst int, per time.Duration) (*rateLimiter, *fixedClock) {
	clock := &fixedClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	limiter := newRateLimiter(burst, per)
	limiter.now = clock.now

	return limiter, clock
}

func TestLimiterAllowsBurstThenBlocks(t *testing.T) {
	limiter, _ := newTestLimiter(3, time.Minute)

	for i := range 3 {
		if allowed, _ := limiter.allow("1.2.3.4"); !allowed {
			t.Fatalf("%d. istek reddedildi, burst 3", i+1)
		}
	}

	allowed, wait := limiter.allow("1.2.3.4")
	if allowed {
		t.Fatal("4. istek geçti — burst aşıldığı hâlde sınır uygulanmadı")
	}
	if wait <= 0 {
		t.Errorf("bekleme süresi = %v, pozitif olmalı (Retry-After'a gidiyor)", wait)
	}
}

// Jetonlar SÜREKLİ doluyor: sabit pencere olsaydı pencere sınırında iki tam
// kota arka arkaya harcanabilirdi.
func TestLimiterRefillsGradually(t *testing.T) {
	limiter, clock := newTestLimiter(6, time.Minute) // 10 saniyede 1 jeton

	for range 6 {
		if allowed, _ := limiter.allow("1.2.3.4"); !allowed {
			t.Fatal("burst içindeki istek reddedildi")
		}
	}

	if allowed, _ := limiter.allow("1.2.3.4"); allowed {
		t.Fatal("kova boşken istek geçti")
	}

	// 10 saniye → tam 1 jeton.
	clock.advance(10 * time.Second)

	if allowed, _ := limiter.allow("1.2.3.4"); !allowed {
		t.Fatal("yenilenen jeton kullanılamadı")
	}
	if allowed, _ := limiter.allow("1.2.3.4"); allowed {
		t.Fatal("tek jeton iki isteğe yetti")
	}
}

// Kova TAVANI AŞMIYOR: uzun süre sessiz kalan bir istemci sınırsız bir
// birikim yapıp tek seferde boşaltamamalı.
func TestLimiterCapsAtBurst(t *testing.T) {
	limiter, clock := newTestLimiter(3, time.Minute)

	clock.advance(24 * time.Hour)

	for i := range 3 {
		if allowed, _ := limiter.allow("1.2.3.4"); !allowed {
			t.Fatalf("%d. istek reddedildi", i+1)
		}
	}
	if allowed, _ := limiter.allow("1.2.3.4"); allowed {
		t.Error("birikmiş jetonlar burst'ü aştı")
	}
}

// Sınır İSTEMCİ BAŞINA: bir istemcinin kotayı bitirmesi diğerlerini
// etkilememeli (yoksa tek bir bot tüm kullanıcıları kilitlerdi).
func TestLimiterIsPerClient(t *testing.T) {
	limiter, _ := newTestLimiter(2, time.Minute)

	for range 2 {
		if allowed, _ := limiter.allow("1.1.1.1"); !allowed {
			t.Fatal("ilk istemcinin burst'ü reddedildi")
		}
	}
	if allowed, _ := limiter.allow("1.1.1.1"); allowed {
		t.Fatal("ilk istemci sınırı aşabildi")
	}

	if allowed, _ := limiter.allow("2.2.2.2"); !allowed {
		t.Error("ikinci istemci, birincinin kotası yüzünden reddedildi")
	}
}

// Boşta kalan kovalar temizleniyor: sözlük her yeni IP için sonsuza kadar
// büyüseydi, sınırlayıcının kendisi bir kaynak tüketimi yüzeyi olurdu.
func TestLimiterEvictsIdleBuckets(t *testing.T) {
	limiter, clock := newTestLimiter(2, time.Minute)

	for i := range 50 {
		limiter.allow("10.0.0." + strconv.Itoa(i))
	}

	if len(limiter.buckets) != 50 {
		t.Fatalf("kova sayısı = %d, beklenen 50", len(limiter.buckets))
	}

	clock.advance(idleBucketTTL + time.Minute)
	limiter.allow("192.168.1.1")

	if len(limiter.buckets) != 1 {
		t.Errorf("kova sayısı = %d, beklenen 1 (boştakiler temizlenmeliydi)", len(limiter.buckets))
	}
}

// --- İstemci kimliği ---------------------------------------------------------

func forwardedRequest(t *testing.T, remoteAddr, forwardedFor string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	req.Header.Set("X-Real-IP", "5.6.7.8")

	return req
}

// VARSAYILAN GÜVENLİ: güvenilen kaynak listesi BOŞKEN başlıklar hiç
// okunmuyor (0.4'ün `middleware.RealIP` kararı). Okunsaydı, her istekte
// farklı bir `X-Forwarded-For` yazan bir istemci her seferinde YENİ BİR KOVA
// açar ve sınır hiç uygulanmazdı.
func TestClientKeyIgnoresForwardedHeaders(t *testing.T) {
	req := forwardedRequest(t, "203.0.113.9:54321", "1.2.3.4")

	if key := (clientIdentifier{}).key(req); key != "203.0.113.9" {
		t.Errorf("anahtar = %q, beklenen 203.0.113.9 (başlıklar yok sayılmalı)", key)
	}
}

// GÜVENİLMEYEN BİR KAYNAKTAN GELEN SAHTE BAŞLIK YOK SAYILIYOR — liste dolu
// olsa bile. `middleware.RealIP`in reddedilme sebebi tam olarak buydu:
// istemci kendi IP'sini uyduramamalı.
func TestClientKeyIgnoresSpoofedForwardedFromUntrustedPeer(t *testing.T) {
	identifier := newClientIdentifier([]string{"10.9.0.1"})

	req := forwardedRequest(t, "203.0.113.9:54321", "1.2.3.4")

	if key := identifier.key(req); key != "203.0.113.9" {
		t.Errorf("anahtar = %q, beklenen 203.0.113.9 — güvenilmeyen kaynağın başlığı okundu", key)
	}
}

// GÜVENİLEN BİR KAYNAKTAN gelen başlık okunuyor: Next sunucusu (SSR) api'yi
// ziyaretçi adına çağırıyor ve gerçek ziyaretçi adresi yalnızca burada.
func TestClientKeyUsesForwardedIPFromTrustedProxy(t *testing.T) {
	identifier := newClientIdentifier([]string{"10.9.0.1"})

	req := forwardedRequest(t, "10.9.0.1:44444", "198.51.100.7")

	if key := identifier.key(req); key != "198.51.100.7" {
		t.Errorf("anahtar = %q, beklenen 198.51.100.7", key)
	}
}

// CIDR yazımı da kabul ediliyor (konteyner ağı tek bir IP değil bir blok).
func TestClientKeyAcceptsTrustedCIDR(t *testing.T) {
	identifier := newClientIdentifier([]string{"172.18.0.0/16"})

	req := forwardedRequest(t, "172.18.0.5:44444", "198.51.100.7")

	if key := identifier.key(req); key != "198.51.100.7" {
		t.Errorf("anahtar = %q, beklenen 198.51.100.7", key)
	}
}

// ZİNCİR SAĞDAN SOLA GEZİLİYOR: güvenilen her durak atlanıyor, ilk
// GÜVENİLMEYEN değer istemci sayılıyor. Soldan okumak, zincirin başına
// istediğini yazan bir istemciye kimliği seçtirirdi.
func TestClientKeySkipsTrustedHopsRightToLeft(t *testing.T) {
	identifier := newClientIdentifier([]string{"10.9.0.1", "10.9.0.2"})

	// İstemcinin uydurduğu değer + gerçek istemci + iki güvenilen durak.
	req := forwardedRequest(t, "10.9.0.1:44444", "9.9.9.9, 198.51.100.7, 10.9.0.2")

	if key := identifier.key(req); key != "198.51.100.7" {
		t.Errorf("anahtar = %q, beklenen 198.51.100.7", key)
	}
}

// Zincirin TAMAMI güvenilen duraklardan oluşuyorsa geriye istemci kalmıyor:
// kaynak proxy'nin kendisi anahtar oluyor (uydurma bir değere düşmüyoruz).
func TestClientKeyFallsBackToPeerWhenChainIsAllTrusted(t *testing.T) {
	identifier := newClientIdentifier([]string{"10.9.0.1", "10.9.0.2"})

	req := forwardedRequest(t, "10.9.0.1:44444", "10.9.0.2")

	if key := identifier.key(req); key != "10.9.0.1" {
		t.Errorf("anahtar = %q, beklenen 10.9.0.1", key)
	}
}

// Bozuk bir liste SESSİZCE "hiçbir kaynağa güvenme"ye düşmemeli: sınır o
// zaman beklenenden başka bir kimlikle çalışır ve kimse fark etmez. Boot
// doğrulaması bu fonksiyona dayanıyor (cmd/api/main.go).
func TestParseTrustedProxiesRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"değil-bir-ip", "10.0.0.0/99", "10.0.0.1/", "1.2.3"} {
		if err := ParseTrustedProxies([]string{raw}); err == nil {
			t.Errorf("%q kabul edildi", raw)
		}
	}

	if err := ParseTrustedProxies([]string{"10.9.0.1", "172.18.0.0/16", "::1"}); err != nil {
		t.Errorf("geçerli liste reddedildi: %v", err)
	}
}

// BULGUNUN KENDİSİ: SSR üzerinden gelen İKİ FARKLI ziyaretçi AYRI kovalara
// düşmeli. Düzeltmeden önce ikisi de Next sunucusunun tek IP'sini
// paylaşıyordu — yani bir ziyaretçinin kotayı bitirmesi HERKESİ kilitliyordu.
func TestForwardedVisitorsGetSeparateBuckets(t *testing.T) {
	identifier := newClientIdentifier([]string{"10.9.0.1"})
	limiter, _ := newTestLimiter(2, time.Minute)

	visitor := func(ip string) bool {
		allowed, _ := limiter.allow(identifier.key(forwardedRequest(t, "10.9.0.1:44444", ip)))

		return allowed
	}

	// Birinci ziyaretçi kotasını bitiriyor.
	for range 2 {
		if !visitor("198.51.100.7") {
			t.Fatal("ilk ziyaretçinin burst'ü reddedildi")
		}
	}
	if visitor("198.51.100.7") {
		t.Fatal("ilk ziyaretçi sınırı aşabildi")
	}

	// İkinci ziyaretçi etkilenmemeli.
	if !visitor("198.51.100.8") {
		t.Error("ikinci ziyaretçi, birincinin kotası yüzünden reddedildi — kovalar hâlâ ortak")
	}
}

// --- Zincirdeki davranış -----------------------------------------------------

// Sınır aşıldığında sözleşme korunuyor: 429 + standart hata zarfı +
// Retry-After.
func TestRateLimitedResponseUsesStandardEnvelope(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	var last *httptest.ResponseRecorder
	// authRateBurst + 1 istek: sonuncusu reddedilmeli.
	for range authRateBurst + 1 {
		last = postJSON(t, router, "/v1/auth/login", `{"email":"yok@example.com","password":"parolaparola"}`)
	}

	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("durum = %d, beklenen 429 (gövde: %s)", last.Code, last.Body.String())
	}

	if retry := last.Header().Get("Retry-After"); retry == "" {
		t.Error("Retry-After başlığı yok")
	} else if seconds, err := strconv.Atoi(retry); err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q, en az 1 saniye olmalı", retry)
	}

	body := decodeErrorResponse(t, last)

	if body.Error.Code != codeRateLimited {
		t.Errorf("hata kodu = %q, beklenen %q", body.Error.Code, codeRateLimited)
	}
	if body.Error.RequestID == "" {
		t.Error("requestId boş — hata zarfının kuralı 429'da da geçerli")
	}
}

// SINIR YALNIZCA KENDİ YÜZEYİNDE: giriş denemeleriyle kotayı bitirmek,
// oturumlu ürün uçlarını kilitlememeli.
func TestRateLimitDoesNotLeakToOtherSurfaces(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	for range authRateBurst + 5 {
		postJSON(t, router, "/v1/auth/login", `{"email":"yok@example.com","password":"parolaparola"}`)
	}

	// /health hiç sınırlı değil: altyapı ucu, orkestratör her 10 saniyede
	// bir yokluyor (0.3).
	rec := do(t, router, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("/health durumu = %d — altyapı ucu sınıra takılmamalı", rec.Code)
	}
}

// PUBLIC OKUMA UCU DA SINIRLI (7.5'in bulgusu).
//
// 7.4 "sınır oturumsuz yüzeylerde" demişti ama bu uç atlanmıştı — oysa
// oturumsuz yüzeylerin EN PAHALISI: istek başına on bir sorgu, 90 günlük özet
// taraması ve `no-store` bir cevap. Kimliksiz bir çağıranın veritabanını
// yüklemesinin en ucuz yolu buydu.
func TestPublicStatusReadIsRateLimited(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	var last *httptest.ResponseRecorder
	for range publicReadRateBurst + 1 {
		last = do(t, router, http.MethodGet, "/v1/public/status/acme", "", nil)
	}

	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("durum = %d, beklenen 429 (public okuma ucu sınırsız)", last.Code)
	}

	if body := decodeErrorResponse(t, last); body.Error.Code != codeRateLimited {
		t.Errorf("hata kodu = %q, beklenen %q", body.Error.Code, codeRateLimited)
	}
}

// Okuma seli, YAZMA yüzeyini kilitlememeli: kovalar ayrı.
func TestPublicReadFloodDoesNotBlockSubscribe(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	for range publicReadRateBurst + 5 {
		do(t, router, http.MethodGet, "/v1/public/status/acme", "", nil)
	}

	rec := postJSON(t, router, "/v1/public/status/acme/subscribers", `{"email":"a@example.com"}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("okuma seli abonelik ucunu bloklandı — kovalar ortak")
	}
}

// ABONELİK SELİ, ABONELİKTEN ÇIKMAYI BLOKLAMAMALI.
//
// İptal linki e-postadaki tek çıkış yolu ve her zaman çalışmak zorunda; onu
// abonelik ucuyla AYNI kovaya koymak, "abone ol"a basan birinin başkalarının
// iptal linkini düşürmesi demekti.
func TestSubscribeFloodDoesNotBlockUnsubscribe(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	var last *httptest.ResponseRecorder
	for range subscribeRateBurst + 3 {
		last = postJSON(t, router, "/v1/public/status/acme/subscribers", `{"email":"a@example.com"}`)
	}

	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("abonelik ucu sınıra takılmadı: durum = %d", last.Code)
	}

	rec := postJSON(t, router, "/v1/public/subscriptions/unsubscribe", `{"token":"deneme"}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("iptal ucu abonelik seliyle bloklandı — kovalar hâlâ ortak")
	}
}
