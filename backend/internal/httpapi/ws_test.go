package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
)

// loopbackNotifier, taşıma yolunun testlerdeki karşılığı: olayı Postgres'e
// göndermek yerine doğrudan hub'a veriyor.
//
// GERÇEK TAŞIMA BURADA TAKLİT EDİLİYOR ama sözleşmesi aynı: olay hub'a
// yayıncıdan geçerek giriyor, handler'dan doğrudan değil. Postgres ayağının
// kendisi gerçek veritabanına karşı internal/realtime'da ölçülüyor.
type loopbackNotifier struct{ hub *realtime.Hub }

func (n *loopbackNotifier) Publish(_ context.Context, channel string, event realtime.Event) error {
	n.hub.Publish(channel, event)

	return nil
}

// wsFixture, WebSocket testlerinin ortak kurulumu.
type wsFixture struct {
	deps   Deps
	server *httptest.Server
	// url, ws:// şemasına çevrilmiş sunucu adresi.
	url string
}

func newWSFixture(t *testing.T) wsFixture {
	t.Helper()

	deps := newTestDeps(t)
	server := httptest.NewServer(NewRouter(deps))
	t.Cleanup(server.Close)

	return wsFixture{
		deps:   deps,
		server: server,
		url:    "ws" + strings.TrimPrefix(server.URL, "http"),
	}
}

// ticketFor, verilen org için doğrudan servis üzerinden bilet üretir (bilet
// UCUNUN testi ayrı: TestRealtimeTicketEndpoint...).
func (f wsFixture) ticketFor(t *testing.T, orgID uuid.UUID) string {
	t.Helper()

	ticket, err := f.deps.Tickets.Issue(uuid.New(), orgID)
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	return ticket.Value
}

// dial, bileti sorgu parametresinde taşıyarak bağlanır.
func (f wsFixture) dial(t *testing.T, ticket string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	return websocket.Dial(t.Context(), f.url+"/v1/ws?ticket="+ticket, &websocket.DialOptions{
		HTTPHeader: header,
	})
}

// dialOK, bağlantının kurulmasını şart koşar ve ilk `realtime.ready` olayını
// tüketir.
func (f wsFixture) dialOK(t *testing.T, ticket string) *websocket.Conn {
	t.Helper()

	conn, _, err := f.dial(t, ticket, nil)
	if err != nil {
		t.Fatalf("bağlanılamadı: %v", err)
	}

	t.Cleanup(func() { _ = conn.CloseNow() })

	if event := readEvent(t, conn); event.Type != eventReady {
		t.Fatalf("ilk olay = %q, beklenen %q", event.Type, eventReady)
	}

	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn) realtime.Event {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("olay okunamadı: %v", err)
	}

	var event realtime.Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("olay çözümlenemedi: %v (%s)", err, data)
	}

	return event
}

func writeMessage(t *testing.T, conn *websocket.Conn, msg clientMessage) {
	t.Helper()

	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("mesaj kodlanamadı: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("mesaj yazılamadı: %v", err)
	}
}

// --- Kimlik doğrulama ---

func TestWebSocketRequiresTicket(t *testing.T) {
	fixture := newWSFixture(t)

	_, resp, err := websocket.Dial(t.Context(), fixture.url+"/v1/ws", nil)
	if err == nil {
		t.Fatal("biletsiz bağlantı kabul edildi")
	}

	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, beklenen 401", resp)
	}
}

func TestWebSocketRejectsInvalidTicket(t *testing.T) {
	fixture := newWSFixture(t)

	_, resp, err := fixture.dial(t, "bu-bir-bilet-degil", nil)
	if err == nil {
		t.Fatal("geçersiz bilet kabul edildi")
	}

	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, beklenen 401", resp)
	}
}

// BİLETİN ASIL ÖZELLİĞİ: aynı bilet ikinci kez kullanılamıyor. Sorgu
// parametresi erişim loguna yazıldığı için, loga düşen dizenin değersiz
// olması buna dayanıyor.
func TestWebSocketTicketCannotBeReplayed(t *testing.T) {
	fixture := newWSFixture(t)

	ticket := fixture.ticketFor(t, uuid.New())

	conn := fixture.dialOK(t, ticket)
	defer func() { _ = conn.CloseNow() }()

	_, resp, err := fixture.dial(t, ticket, nil)
	if err == nil {
		t.Fatal("kullanılmış bilet ikinci kez kabul edildi")
	}

	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, beklenen 401", resp)
	}
}

func TestWebSocketRejectsExpiredTicket(t *testing.T) {
	fixture := newWSFixture(t)

	// Süresi dolmuş bir bilet üretmek için ayrı, ömrü çok kısa bir servis
	// gerekiyor — ama router'ın servisiyle AYNI SIRRI paylaşıyor, yani imza
	// geçerli. Reddin sebebi yalnızca süre.
	shortLived := auth.NewTicketService([]byte(testJWTSecret), time.Millisecond)

	ticket, err := shortLived.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	_, resp, err := fixture.dial(t, ticket.Value, nil)
	if err == nil {
		t.Fatal("süresi dolmuş bilet kabul edildi")
	}

	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, beklenen 401", resp)
	}
}

// --- Origin denetimi ---

// CORS WEBSOCKET'E UYGULANMIYOR: tarayıcı el sıkışmayı same-origin
// politikasına tabi tutmuyor. Denetim bu yüzden ELLE yazıldı; testi de bu.
func TestWebSocketRejectsForeignOrigin(t *testing.T) {
	fixture := newWSFixture(t)

	header := http.Header{}
	header.Set("Origin", "http://kotu-site.example")

	_, resp, err := fixture.dial(t, fixture.ticketFor(t, uuid.New()), header)
	if err == nil {
		t.Fatal("yabancı origin'den bağlantı kabul edildi")
	}

	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, beklenen 403", resp)
	}
}

func TestWebSocketAcceptsConfiguredOrigin(t *testing.T) {
	fixture := newWSFixture(t)

	header := http.Header{}
	header.Set("Origin", testWebOrigin)

	conn, _, err := fixture.dial(t, fixture.ticketFor(t, uuid.New()), header)
	if err != nil {
		t.Fatalf("izinli origin reddedildi: %v", err)
	}

	defer func() { _ = conn.CloseNow() }()

	if event := readEvent(t, conn); event.Type != eventReady {
		t.Errorf("ilk olay = %q", event.Type)
	}
}

// REDDEDİLEN EL SIKIŞMA BİLETİ HARCAMAMALI: origin denetimi bilet
// doğrulamasından ÖNCE çalışıyor, yoksa yanlış origin'den gelen bir istek
// kullanıcının biletini yakardı.
func TestRejectedOriginDoesNotBurnTicket(t *testing.T) {
	fixture := newWSFixture(t)

	ticket := fixture.ticketFor(t, uuid.New())

	header := http.Header{}
	header.Set("Origin", "http://kotu-site.example")

	if _, _, err := fixture.dial(t, ticket, header); err == nil {
		t.Fatal("yabancı origin'den bağlantı kabul edildi")
	}

	conn := fixture.dialOK(t, ticket)
	_ = conn.CloseNow()
}

// --- ping / echo ---

func TestWebSocketPingIsAnsweredDirectly(t *testing.T) {
	fixture := newWSFixture(t)

	conn := fixture.dialOK(t, fixture.ticketFor(t, uuid.New()))

	writeMessage(t, conn, clientMessage{Type: "ping"})

	if event := readEvent(t, conn); event.Type != eventPong {
		t.Fatalf("cevap = %q, beklenen %q", event.Type, eventPong)
	}
}

// PING ORG KANALINA GİRMİYOR: sorulan şey "bu BAĞLANTI çalışıyor mu".
// Kanaldan geçseydi aynı org'un diğer sekmeleri de bir pong görürdü.
func TestPingIsNotBroadcastToOrg(t *testing.T) {
	fixture := newWSFixture(t)

	orgID := uuid.New()

	first := fixture.dialOK(t, fixture.ticketFor(t, orgID))
	second := fixture.dialOK(t, fixture.ticketFor(t, orgID))

	writeMessage(t, first, clientMessage{Type: "ping"})

	if event := readEvent(t, first); event.Type != eventPong {
		t.Fatalf("gönderen için cevap = %q", event.Type)
	}

	// İkinci sekmeye echo göndererek sıranın gerçekten boş olduğunu ölç: pong
	// yayımlanmış olsaydı ilk okunan olay o olurdu.
	writeMessage(t, second, clientMessage{Type: "echo", Message: "merhaba"})

	if event := readEvent(t, second); event.Type != eventEcho {
		t.Fatalf("ikinci bağlantının ilk olayı = %q, beklenen %q", event.Type, eventEcho)
	}
}

// UÇTAN UCA: echo mesajı yayıncıdan geçip hub üzerinden AYNI ORG'un tüm
// bağlantılarına dağılıyor.
func TestEchoReachesEveryConnectionInOrg(t *testing.T) {
	fixture := newWSFixture(t)

	orgID := uuid.New()

	sender := fixture.dialOK(t, fixture.ticketFor(t, orgID))
	other := fixture.dialOK(t, fixture.ticketFor(t, orgID))

	writeMessage(t, sender, clientMessage{Type: "echo", Message: "merhaba"})

	for name, conn := range map[string]*websocket.Conn{"gönderen": sender, "diğer": other} {
		event := readEvent(t, conn)
		if event.Type != eventEcho {
			t.Fatalf("%s: olay = %q, beklenen %q", name, event.Type, eventEcho)
		}

		var data struct {
			Message string `json:"message"`
			From    string `json:"from"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("%s: gövde çözümlenemedi: %v", name, err)
		}

		if data.Message != "merhaba" {
			t.Errorf("%s: mesaj = %q", name, data.Message)
		}
	}
}

// 6.1'İN ASIL GARANTİSİ: bir org'un olayı başka bir org'un bağlantısına ASLA
// gitmiyor.
func TestEventsDoNotLeakBetweenOrgs(t *testing.T) {
	fixture := newWSFixture(t)

	orgA, orgB := uuid.New(), uuid.New()

	connA := fixture.dialOK(t, fixture.ticketFor(t, orgA))
	connB := fixture.dialOK(t, fixture.ticketFor(t, orgB))

	writeMessage(t, connA, clientMessage{Type: "echo", Message: "sadece-a"})

	if event := readEvent(t, connA); event.Type != eventEcho {
		t.Fatalf("A'nın olayı = %q", event.Type)
	}

	// B'nin sırasında A'nın olayı OLMAMALI. B kendi echo'sunu gönderiyor:
	// sızıntı olsaydı ilk okunan olay A'nınki olurdu.
	writeMessage(t, connB, clientMessage{Type: "echo", Message: "sadece-b"})

	event := readEvent(t, connB)

	var data struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}

	if data.Message != "sadece-b" {
		t.Fatalf("B'nin gördüğü mesaj = %q — başka org'un olayı sızdı", data.Message)
	}
}

func TestWebSocketRejectsUnknownMessageType(t *testing.T) {
	fixture := newWSFixture(t)

	conn := fixture.dialOK(t, fixture.ticketFor(t, uuid.New()))

	writeMessage(t, conn, clientMessage{Type: "bilinmeyen"})

	if event := readEvent(t, conn); event.Type != eventError {
		t.Fatalf("cevap = %q, beklenen %q", event.Type, eventError)
	}
}

func TestWebSocketRejectsOversizedEcho(t *testing.T) {
	fixture := newWSFixture(t)

	conn := fixture.dialOK(t, fixture.ticketFor(t, uuid.New()))

	writeMessage(t, conn, clientMessage{Type: "echo", Message: strings.Repeat("x", maxEchoMessage+1)})

	if event := readEvent(t, conn); event.Type != eventError {
		t.Fatalf("cevap = %q, beklenen %q", event.Type, eventError)
	}
}

// Bozuk bir mesaj bağlantıyı düşürmemeli: istemcinin bir hatası tüm canlı
// akışını kaybetmesine yol açmasın.
func TestMalformedMessageDoesNotCloseConnection(t *testing.T) {
	fixture := newWSFixture(t)

	conn := fixture.dialOK(t, fixture.ticketFor(t, uuid.New()))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, []byte("{bozuk")); err != nil {
		t.Fatalf("mesaj yazılamadı: %v", err)
	}

	writeMessage(t, conn, clientMessage{Type: "ping"})

	if event := readEvent(t, conn); event.Type != eventPong {
		t.Fatalf("cevap = %q, beklenen %q", event.Type, eventPong)
	}
}

// --- Hub ile bağlantının uyumu ---

// Hub kapandığında (api kapanıyor) bağlantı "going away" ile kapanmalı:
// istemci sebebi bilerek yeniden bağlansın.
func TestShutdownClosesConnectionWithGoingAway(t *testing.T) {
	fixture := newWSFixture(t)

	conn := fixture.dialOK(t, fixture.ticketFor(t, uuid.New()))

	fixture.deps.Realtime.Shutdown()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, _, err := conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("kapanış kodu = %d, beklenen %d (hata: %v)", got, websocket.StatusGoingAway, err)
	}
}

// Bağlantı kapandığında abonelik hub'dan DÜŞMELİ; aksi hâlde her kapanan
// sekme hub'da bir kayıt bırakırdı.
func TestClosingConnectionRemovesSubscription(t *testing.T) {
	fixture := newWSFixture(t)

	orgID := uuid.New()
	channel := realtime.OrgChannel(orgID)

	conn := fixture.dialOK(t, fixture.ticketFor(t, orgID))

	if got := fixture.deps.Realtime.Subscribers(channel); got != 1 {
		t.Fatalf("abone sayısı = %d, beklenen 1", got)
	}

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("bağlantı kapatılamadı: %v", err)
	}

	// Sunucu tarafındaki temizlik asenkron: kısa bir süre içinde düşmeli.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.deps.Realtime.Subscribers(channel) == 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("abonelik düşmedi: %d abone kaldı", fixture.deps.Realtime.Subscribers(channel))
}

// --- Bilet ucu ---

func TestRealtimeTicketEndpointRequiresMembership(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	// Üye olmayan kullanıcı org'u hiç göremiyor (1.4'ün 404 disiplini).
	rec := do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/realtime-ticket", "", bearer(fixture.outsider))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, beklenen 404 (%s)", rec.Code, rec.Body.String())
	}

	// Kimliksiz istek 401.
	rec = do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/realtime-ticket", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRealtimeTicketEndpointIssuesScopedTicket(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	rec := do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/realtime-ticket", "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	var body ticketResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}

	if body.Ticket == "" {
		t.Fatal("bilet boş döndü")
	}

	if body.ExpiresIn != int64(auth.DefaultTicketTTL.Seconds()) {
		t.Errorf("expiresIn = %d", body.ExpiresIn)
	}
}

// recordingBroadcaster, duyuruları kaydeden sahte yayıncı (6.2).
type recordingBroadcaster struct {
	mu    sync.Mutex
	calls []recordedBroadcast
}

type recordedBroadcast struct {
	orgID    uuid.UUID
	monitors []uuid.UUID
	reason   string
}

func (b *recordingBroadcaster) Changed(_ context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.calls = append(b.calls, recordedBroadcast{orgID: orgID, monitors: monitorIDs, reason: reason})
}

func (b *recordingBroadcaster) snapshot() []recordedBroadcast {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]recordedBroadcast(nil), b.calls...)
}

// --- Public (oturumsuz) kanal — 6.2 ------------------------------------------

// publishedFixture, yayımlanmış bir status sayfası olan bir kurulum döner.
func (f wsFixture) publishPage(t *testing.T, slug string) uuid.UUID {
	t.Helper()

	pages, ok := f.deps.StatusPages.(*fakeStatusPageService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	orgID := uuid.New()
	pages.register(orgID, slug)
	pages.pages[orgID].page.IsPublished = true

	return orgID
}

func (f wsFixture) dialPublic(t *testing.T, slug string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	return websocket.Dial(t.Context(), f.url+"/v1/public/ws?slug="+slug, nil)
}

func (f wsFixture) dialPublicOK(t *testing.T, slug string) *websocket.Conn {
	t.Helper()

	conn, _, err := f.dialPublic(t, slug)
	if err != nil {
		t.Fatalf("public kanala bağlanılamadı: %v", err)
	}

	t.Cleanup(func() { _ = conn.CloseNow() })

	if event := readEvent(t, conn); event.Type != eventReady {
		t.Fatalf("ilk olay = %q, beklenen %q", event.Type, eventReady)
	}

	return conn
}

// PUBLIC KANAL KİMLİK DOĞRULAMASIZ: bilet yok, token yok — kimliği yoldaki
// slug taşıyor, tıpkı 5.1'in okuma ucunda olduğu gibi.
func TestPublicWebSocketNeedsNoAuthentication(t *testing.T) {
	fixture := newWSFixture(t)
	fixture.publishPage(t, "acme")

	conn := fixture.dialPublicOK(t, "acme")
	_ = conn.CloseNow()
}

// YAYIMLANMAMIŞ ≡ OLMAYAN (5.1): ikisi de aynı 404. Fark üretmek, denenen
// slug'lardan hangi org'ların kayıtlı olduğunu sızdırırdı.
func TestPublicWebSocketRejectsUnpublishedAndUnknownSlugIdentically(t *testing.T) {
	fixture := newWSFixture(t)

	pages, ok := fixture.deps.StatusPages.(*fakeStatusPageService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}
	pages.register(uuid.New(), "gizli") // yayımlanmamış

	for _, slug := range []string{"gizli", "hic-olmayan", ""} {
		_, resp, err := fixture.dialPublic(t, slug)
		if err == nil {
			t.Fatalf("%q için bağlantı kabul edildi", slug)
		}

		if resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%q için status = %v, beklenen 404", slug, resp)
		}
	}
}

// PUBLIC KANALDA ORIGIN DENETİMİ YOK — bilinçli: ortada ambient kimlik
// bilgisiyle ele geçirilecek bir oturum yok ve denetim, 5.4'ün alt alan
// adından (acme.localhost:3100) erişimini kırardı.
func TestPublicWebSocketAcceptsAnyOrigin(t *testing.T) {
	fixture := newWSFixture(t)
	fixture.publishPage(t, "acme")

	header := http.Header{}
	header.Set("Origin", "http://acme.localhost:3100")

	conn, _, err := websocket.Dial(t.Context(), fixture.url+"/v1/public/ws?slug=acme",
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("alt alan adından bağlanılamadı: %v", err)
	}

	defer func() { _ = conn.CloseNow() }()

	if event := readEvent(t, conn); event.Type != eventReady {
		t.Errorf("ilk olay = %q", event.Type)
	}
}

// 6.2'NİN İZOLASYON TESTİ: bir sayfanın olayı başka bir sayfanın kanalına
// GİTMİYOR.
func TestPublicChannelsAreIsolated(t *testing.T) {
	fixture := newWSFixture(t)
	fixture.publishPage(t, "acme")
	fixture.publishPage(t, "globex")

	acme := fixture.dialPublicOK(t, "acme")
	globex := fixture.dialPublicOK(t, "globex")

	event, err := realtime.NewEvent(realtime.EventStatusChanged, time.Now(),
		map[string]string{"reason": realtime.ReasonIncidentOpened})
	if err != nil {
		t.Fatalf("olay üretilemedi: %v", err)
	}

	fixture.deps.Realtime.Publish(realtime.PublicChannel("acme"), event)

	if got := readEvent(t, acme); got.Type != realtime.EventStatusChanged {
		t.Fatalf("acme olayı = %q", got.Type)
	}

	// globex'in sırası boş olmalı: kendi kanalına gönderilen olayı ilk sırada
	// görmeli.
	own, err := realtime.NewEvent("globex.kendi", time.Now(), nil)
	if err != nil {
		t.Fatalf("olay üretilemedi: %v", err)
	}

	fixture.deps.Realtime.Publish(realtime.PublicChannel("globex"), own)

	if got := readEvent(t, globex); got.Type != "globex.kendi" {
		t.Fatalf("globex'in ilk olayı = %q — başka sayfanın olayı sızdı", got.Type)
	}
}

// ORG KANALI İLE PUBLIC KANAL AYRI: oturumlu panoya giden bir olay public
// sayfaya gitmiyor (kapıdan geçmediyse).
func TestOrgChannelEventDoesNotReachPublicChannel(t *testing.T) {
	fixture := newWSFixture(t)
	orgID := fixture.publishPage(t, "acme")

	public := fixture.dialPublicOK(t, "acme")

	event, err := realtime.NewEvent(realtime.EventStatusChanged, time.Now(),
		map[string]string{"reason": realtime.ReasonMonitorStatus})
	if err != nil {
		t.Fatalf("olay üretilemedi: %v", err)
	}

	fixture.deps.Realtime.Publish(realtime.OrgChannel(orgID), event)

	marker, err := realtime.NewEvent("public.isaret", time.Now(), nil)
	if err != nil {
		t.Fatalf("olay üretilemedi: %v", err)
	}

	fixture.deps.Realtime.Publish(realtime.PublicChannel("acme"), marker)

	if got := readEvent(t, public); got.Type != "public.isaret" {
		t.Fatalf("public kanalın ilk olayı = %q — org kanalının olayı sızdı", got.Type)
	}
}

// Manuel incident açmak canlı yayına duyuruluyor ve kapıya KAPSANAN
// SERVİSLERİN listesi veriliyor (public görünürlük kararı ona dayanıyor).
func TestManualIncidentBroadcastsAffectedMonitors(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	broadcaster, ok := fixture.broadcaster()
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	monitorID := uuid.New()

	body := `{"title":"Kesinti","impact":"major","monitorIds":["` + monitorID.String() + `"],"body":"inceleniyor"}`

	rec := do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/incidents", body, bearer(fixture.ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	calls := broadcaster.snapshot()
	if len(calls) != 1 {
		t.Fatalf("duyuru sayısı = %d, beklenen 1", len(calls))
	}

	if calls[0].reason != realtime.ReasonIncidentCreated {
		t.Errorf("sebep = %q, beklenen %q", calls[0].reason, realtime.ReasonIncidentCreated)
	}

	if len(calls[0].monitors) != 1 || calls[0].monitors[0] != monitorID {
		t.Errorf("kapıya verilen servisler = %v, beklenen [%s]", calls[0].monitors, monitorID)
	}
}
