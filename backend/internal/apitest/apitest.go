// Package apitest, api'yi UÇTAN UCA ayağa kaldıran integration test
// katmanıdır (7.1).
//
// Buradaki testlerin diğerlerinden farkı, hiçbir yerinde sahte olmaması:
// gerçek `httpapi.NewRouter` (tüm middleware zinciriyle), gerçek servisler,
// gerçek veritabanı ve gerçek bir TCP dinleyicisi (`httptest.NewServer`).
// Yani ölçtükleri şey "handler ne döndürdü" değil, "istemci ne gördü":
// yönlendirme, middleware sırası, JSON kodlaması, durum kodu ve cookie'ler
// dahil.
//
// NEDEN AYRI BİR PAKET: `internal/httpapi` testleri ROUTER'ı gerçek kuruyor
// ama servisleri SAHTE veriyor — konuları HTTP sözleşmesi (durum kodları,
// hata zarfı, cookie bayrakları) ve sahteler o soruları hızlı cevaplıyor.
// Buradaki testlerin konusu ise katmanların BİRLİKTE doğru çalışması. İkisini
// tek pakette toplamak, aynı dosyada hem sahte hem gerçek servis kurulumu
// tutmak demekti.
//
// KURULUM ÇOĞALTILMIYOR: bağımlılıklar `cmd/api/main.go` ile aynı yapıcı
// fonksiyonlardan geçiyor (bkz. newDeps). Testlerin kendi "yarım uygulama"
// kopyasını tutması router.go'da açıkça reddedilmiş bir şey — prod'da olan
// bir middleware testte sessizce eksik kalırdı.
package apitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/billing"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/httpapi"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/maintenance"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/passwordreset"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/sessions"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/subscribers"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// testWebOrigin, CORS ve WebSocket Origin denetiminin izinli tek adresi.
const testWebOrigin = "http://localhost:3100"

// testPublicBaseURL, abonelik e-postalarındaki linklerin tabanı (6.4).
const testPublicBaseURL = "http://localhost:3100"

// İmzalama sırları: yalnızca bu süreçte yaşıyorlar. Uzunlukları config'in
// 32 karakter alt sınırının üstünde — gerçek doğrulamadan geçsinler.
const (
	testAccessSecret  = "integration-test-access-secret-0123456789"
	testRefreshSecret = "integration-test-refresh-secret-0123456789"
)

// testDialPolicy, SSRF kapısı AÇIK bir politika (7.4).
//
// Integration testlerinin hedefi `httptest` sunucuları, yani loopback —
// üretim politikası onları reddederdi. Korumanın KENDİSİ internal/safedial'da
// ölçülüyor; burada ölçülen şey uygulamanın akışı.
var testDialPolicy = safedial.Policy{AllowPrivate: true}

// uniqueCounter, testler arasında çakışmayan e-posta/ad üretmek için.
var uniqueCounter atomic.Uint64

// Harness, ayakta duran bir api ve ona bağlı yardımcılar.
type Harness struct {
	t *testing.T

	// BaseURL, dinleyen sunucunun adresi (`http://127.0.0.1:<port>`).
	BaseURL string
	// Handler, sunucunun arkasındaki router. Testler bunu yalnızca KAYITLI
	// ROTALARI gezmek için kullanıyor (chi.Walk); istekler her zaman gerçek
	// sunucuya gidiyor.
	Handler http.Handler
	// Pool ve Queries, testin veritabanına DOĞRUDAN bakabilmesi için:
	// "api ne döndürdü" ile "veritabanına ne yazıldı" ayrı sorular.
	Pool    *pgxpool.Pool
	Queries *store.Queries
	// Client, çerezleri TAŞIMAYAN bir istemci: refresh cookie'sinin gerçekten
	// gönderilip gönderilmediği testin açık bir kararı olsun.
	Client *http.Client
}

// New, gerçek bağımlılıklarla ayağa kalkmış bir api döner.
//
// Veritabanı `uptime_mini_test_api`; her çağrıda tabloları temizlenir
// (bkz. internal/testdb). DATABASE_URL yoksa veya `-short` verilmişse test
// atlanır.
func New(t *testing.T) *Harness {
	t.Helper()

	pool := testdb.Pool(t, "api")
	queries := store.New(pool)

	handler := httpapi.NewRouter(newDeps(pool, queries))

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &Harness{
		t:       t,
		BaseURL: server.URL,
		Handler: handler,
		Pool:    pool,
		Queries: queries,
		Client: &http.Client{
			Timeout: 30 * time.Second,
			// Yönlendirme İZLENMİYOR: api hiçbir yere yönlendirmiyor ve
			// sessizce izlenen bir 3xx testin gördüğü durumu değiştirirdi.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// cheapHasher, testlerin parola hash'leyicisi.
//
// ARGON2 PARAMETRELERİ BİLİNÇLİ OLARAK UCUZ: üretim ayarı (19 MiB, t=2) her
// kayıt/giriş için ~50 ms demek ve bu paket onlarca kullanıcı kaydediyor.
// Hash'in KENDİSİ (PHC kodlaması, doğrulama, 72-byte tuzağının olmaması)
// internal/auth'un kendi testlerinin konusu.
//
// TEK YERDE tanımlı: kayıt/giriş ile parola sıfırlama (8.4) AYNI
// hash'leyiciyi kullanmak zorunda — ayrışsalardı sıfırlanan parolayla giriş
// yapılamazdı ve bunu ancak akışın tamamını koşan bir test yakalardı.
func cheapHasher() auth.Hasher {
	return auth.Hasher{Params: auth.Argon2Params{
		MemoryKiB:   64,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}}
}

// newDeps, cmd/api/main.go ile AYNI yapıcı fonksiyonları çağırır. Tek farkı
// yapılandırmanın ortamdan değil buradan gelmesi ve parola hash'inin ucuz
// parametrelerle koşması (yukarıya bkz.).
func newDeps(pool *pgxpool.Pool, queries *store.Queries) httpapi.Deps {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	hub := realtime.NewHub(logger)
	statusPages := statuspage.NewService(pool, queries, logger)
	// GELİŞTİRİCİ POSTA KUTUSU (8.2) burada AÇIK: integration katmanının
	// konusu "katmanlar birlikte çalışıyor mu" ve posta kutusu, gönderim
	// yolunun gerçekten sonuna kadar gittiğinin tek uçtan-uca kanıtı.
	// Üretimdeki kapı (APP_ENV) burada yok çünkü bu paket zaten test.
	outbox := notifications.NewOutbox(queries)
	mailRegistry := notifications.NewRegistry(testDialPolicy, outbox, logger)

	// Kayıt SIRASINDA hata dönmesi imkânsız: e-posta adapter'ı registry'nin
	// varsayılanlarından biri. Yine de yutulmuyor.
	mailer, err := mailRegistry.For(store.NotificationChannelKindEmail)
	if err != nil {
		panic(fmt.Sprintf("e-posta adapter'ı bulunamadı: %v", err))
	}

	notifier := realtime.NewNotifier(queries, logger)

	// Ödeme sağlayıcısı (8.5): mock — bu projede gerçek sağlayıcı yok.
	payments := billing.NewProvider(logger)

	// Tek orgs servisi, iki rol (8.3): üyelik doğrulaması ve davet akışı.
	orgService := orgs.NewService(pool, queries, mailer, payments, logger)

	return httpapi.Deps{
		Logger:    logger,
		StartedAt: time.Now(),
		DB:        pool,
		Users:     queries,
		Hasher:    cheapHasher(),
		Tokens:    auth.NewTokenService([]byte(testAccessSecret), 15*time.Minute),
		Sessions: sessions.NewService(pool, queries,
			auth.NewRefreshTokenFactory([]byte(testRefreshSecret), 30*24*time.Hour), logger),
		Orgs: orgService,
		// Davetler (8.3): aynı servis örneği, iki uç grubu.
		Invitations: orgService,
		Plans:       orgService,
		// Parola sıfırlama (8.4): hash'leyici kayıt/giriş ile aynı olmalı,
		// yoksa sıfırlanan parolayla giriş yapılamazdı — testin ölçtüğü
		// şeylerden biri tam olarak bu.
		PasswordReset: passwordreset.NewService(
			pool, queries, cheapHasher(), mailer, testPublicBaseURL, logger),
		PublicBaseURL: testPublicBaseURL,
		Monitors:      monitors.NewService(pool, queries, logger),
		// GERÇEK checker: manuel kontrol ucu gerçekten dışarıya istek atıyor.
		// Testler hedef olarak kendi ayağa kaldırdıkları bir sunucuyu veriyor
		// — dış dünyaya bağımlılık yok.
		Checks:        checker.New(testDialPolicy),
		Results:       results.NewService(queries, logger),
		Incidents:     incidents.NewService(pool, queries, logger),
		Maintenance:   maintenance.NewService(pool, queries, logger),
		StatusPages:   statusPages,
		Notifications: notifications.NewService(queries, mailRegistry, logger),
		Subscribers:   subscribers.NewService(queries, mailer, testPublicBaseURL, logger),
		Outbox:        outbox,
		Realtime:      hub,
		Tickets:       auth.NewTicketService([]byte(testAccessSecret), auth.DefaultTicketTTL),
		// TAŞIMA YOLU DA GERÇEK: olay Postgres LISTEN/NOTIFY'a yazılıyor.
		// Dinleyici burada AYAĞA KALDIRILMIYOR — hub'a olay girmesi 6.1'in
		// kendi testlerinin konusu; buradaki testler yayının kesinti yazmayı
		// bloke etmediğini görüyor.
		Notifier:    notifier,
		Broadcaster: realtime.NewBroadcaster(notifier, statusPages, logger),
		WebOrigins:  []string{testWebOrigin},
		// Secure cookie KAPALI: test sunucusu http üzerinde koşuyor.
		// Bayrağın üretimde açıldığı internal/httpapi'de ölçülüyor.
		SecureCookies: false,
	}
}

// --- İstek/cevap ------------------------------------------------------------

// Request, tek bir HTTP çağrısının tarifi.
type Request struct {
	Method string
	Path   string
	Body   string
	// Token, `Authorization: Bearer` başlığına yazılır (boşsa yazılmaz).
	Token   string
	Headers map[string]string
}

// Response, istemcinin gördüğü cevap.
type Response struct {
	Status  int
	Body    []byte
	Header  http.Header
	Cookies []*http.Cookie
}

// Do, isteği gerçek sunucuya gönderir.
func (h *Harness) Do(req Request) Response {
	h.t.Helper()

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(h.t.Context(), req.Method, h.BaseURL+req.Path, body)
	if err != nil {
		h.t.Fatalf("istek kurulamadı: %v", err)
	}

	if req.Body != "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	}
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := h.Client.Do(httpReq)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("cevap gövdesi okunamadı: %v", err)
	}

	return Response{
		Status:  resp.StatusCode,
		Body:    payload,
		Header:  resp.Header,
		Cookies: resp.Cookies(),
	}
}

// JSON, gövdeyi çözer.
func (h *Harness) JSON(resp Response, into any) {
	h.t.Helper()

	if err := json.Unmarshal(resp.Body, into); err != nil {
		h.t.Fatalf("cevap çözülemedi (%s): %v", resp.Body, err)
	}
}

// Expect, beklenen durum kodunu doğrular ve cevabı geri verir.
func (h *Harness) Expect(resp Response, status int) Response {
	h.t.Helper()

	if resp.Status != status {
		h.t.Fatalf("durum = %d, beklenen %d (gövde: %s)", resp.Status, status, resp.Body)
	}

	return resp
}

// ErrorCode, hata zarfındaki `code` alanını döner (1.1'in tek zarfı).
func (h *Harness) ErrorCode(resp Response) string {
	h.t.Helper()

	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	h.JSON(resp, &envelope)

	return envelope.Error.Code
}

// --- Kullanıcı / organizasyon / monitor -------------------------------------

// User, kayıt olmuş bir kullanıcı ve elindeki access token.
type User struct {
	ID    uuid.UUID
	Email string
	Token string
}

// Org, oluşturulmuş bir organizasyon.
type Org struct {
	ID   uuid.UUID
	Slug string
}

// Monitor, oluşturulmuş bir monitor.
type Monitor struct {
	ID   uuid.UUID
	Name string
}

// Register, yeni bir kullanıcı kaydeder ve access token'ıyla döner.
// E-posta her çağrıda benzersiz.
func (h *Harness) Register() User {
	h.t.Helper()

	email := fmt.Sprintf("user%d@example.test", uniqueCounter.Add(1))

	resp := h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   "/v1/auth/register",
		Body: fmt.Sprintf(
			`{"email":%q,"password":"correct-horse-battery","name":"Test Kullanıcı"}`, email),
	}), http.StatusCreated)

	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		AccessToken string `json:"accessToken"`
	}
	h.JSON(resp, &payload)

	id, err := uuid.Parse(payload.User.ID)
	if err != nil {
		h.t.Fatalf("kullanıcı kimliği çözülemedi: %v", err)
	}

	return User{ID: id, Email: email, Token: payload.AccessToken}
}

// CreateOrg, kullanıcının adına bir organizasyon açar (kurucu = owner).
func (h *Harness) CreateOrg(user User) Org {
	h.t.Helper()

	name := fmt.Sprintf("Test Org %d", uniqueCounter.Add(1))

	resp := h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   "/v1/orgs",
		Token:  user.Token,
		Body:   fmt.Sprintf(`{"name":%q}`, name),
	}), http.StatusCreated)

	var payload struct {
		Organization struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"organization"`
	}
	h.JSON(resp, &payload)

	id, err := uuid.Parse(payload.Organization.ID)
	if err != nil {
		h.t.Fatalf("organizasyon kimliği çözülemedi: %v", err)
	}

	org := Org{ID: id, Slug: payload.Organization.Slug}

	// TEST ORGANİZASYONLARI **PRO** PLANDA (8.5).
	//
	// Bu paketin testleri 30 saniyelik kontrol aralığı kullanıyor: kesintinin
	// gerçekten oluştuğunu görmek için scheduler'ın birkaç tur dönmesi
	// gerekiyor ve FREE planın 300 saniyelik alt sınırı bunu dakikalara
	// çıkarırdı.
	//
	// KURAL ESNETİLMİYOR, YAPILANDIRMA KULLANILIYOR — 7.2'nin `failureThreshold`
	// kararının aynısı: PRO planda 30 saniye ÜRÜNÜN meşru bir hâli, testin
	// kendine açtığı bir istisna değil. Kotanın KENDİSİ ayrıca ölçülüyor
	// (bkz. plan_test.go): orada plan bilinçli olarak FREE bırakılıyor.
	if _, err := h.Queries.UpdateOrganizationPlan(h.t.Context(), store.UpdateOrganizationPlanParams{
		ID:   id,
		Plan: store.OrgPlanPro,
	}); err != nil {
		h.t.Fatalf("test organizasyonu PRO plana alınamadı: %v", err)
	}

	return org
}

// AddMember, organizasyona verilen rolde bir üye ekler.
//
// Uç üzerinden yapılamıyor: davet/üye ekleme ucu MVP kapsamında YOK (0.1).
// Doğrudan yazmak burada meşru — testin konusu üyeliğin nasıl doğduğu değil,
// ROLÜN uçlarda nasıl uygulandığı.
func (h *Harness) AddMember(org Org, user User, role store.OrgRole) {
	h.t.Helper()

	if _, err := h.Queries.AddOrganizationMember(h.t.Context(), store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: user.ID,
		Role:   role,
	}); err != nil {
		h.t.Fatalf("üye eklenemedi: %v", err)
	}
}

// CreateMonitor, organizasyona varsayılan ayarlarla bir monitor ekler.
func (h *Harness) CreateMonitor(user User, org Org, name, targetURL string) Monitor {
	h.t.Helper()

	return h.CreateMonitorWith(user, org, fmt.Sprintf(`{"name":%q,"url":%q}`, name, targetURL))
}

// CreateMonitorWith, gövdesi verilen bir monitor ekler (eşikleri değiştiren
// testler için).
func (h *Harness) CreateMonitorWith(user User, org Org, body string) Monitor {
	h.t.Helper()

	resp := h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/v1/orgs/%s/monitors", org.ID),
		Token:  user.Token,
		Body:   body,
	}), http.StatusCreated)

	var payload struct {
		Monitor struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"monitor"`
	}
	h.JSON(resp, &payload)

	id, err := uuid.Parse(payload.Monitor.ID)
	if err != nil {
		h.t.Fatalf("monitor kimliği çözülemedi: %v", err)
	}

	return Monitor{ID: id, Name: payload.Monitor.Name}
}

// PublishStatusPage, verilen monitor'leri yayımlar ve sayfayı yayına açar.
func (h *Harness) PublishStatusPage(user User, org Org, monitorIDs ...uuid.UUID) {
	h.t.Helper()

	items := make([]string, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		items = append(items, fmt.Sprintf(`{"monitorId":%q}`, id))
	}

	h.Expect(h.Do(Request{
		Method: http.MethodPut,
		Path:   fmt.Sprintf("/v1/orgs/%s/status-page/services", org.ID),
		Token:  user.Token,
		Body:   fmt.Sprintf(`{"groups":[],"items":[%s]}`, strings.Join(items, ",")),
	}), http.StatusOK)

	h.Expect(h.Do(Request{
		Method: http.MethodPatch,
		Path:   fmt.Sprintf("/v1/orgs/%s/status-page", org.ID),
		Token:  user.Token,
		Body:   `{"isPublished":true}`,
	}), http.StatusOK)
}

// StatusPageSlug, organizasyonun public slug'ını döner.
func (h *Harness) StatusPageSlug(user User, org Org) string {
	h.t.Helper()

	resp := h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/status-page", org.ID),
		Token:  user.Token,
	}), http.StatusOK)

	var payload struct {
		StatusPage struct {
			Slug string `json:"slug"`
		} `json:"statusPage"`
	}
	h.JSON(resp, &payload)

	return payload.StatusPage.Slug
}

// BodyContains, gövdede bir metnin geçip geçmediğini söyler (sızıntı
// testlerinde cevabın TAMAMI taranıyor, alan alan değil — 5.1'in disiplini).
func BodyContains(body []byte, needle string) bool {
	return bytes.Contains(body, []byte(needle))
}
