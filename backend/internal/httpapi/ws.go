package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
)

// WebSocket yüzeyi (Faz 6.1).
//
// İKİ UÇ, İKİ AYRI YER:
//
//	POST /v1/orgs/{orgID}/realtime-ticket → org-scoped ağacın İÇİNDE
//	GET  /v1/ws?ticket=…                  → istek zaman aşımının DIŞINDA
//
// Bilet ucunun org ağacında olması tesadüf değil: üyelik denetimi 1.4'ün
// `requireOrgMembership` middleware'i tarafından VERİTABANINA KARŞI orada
// yapılıyor ve doğrulanmış org kimliği biletin içine yazılıyor. El sıkışmada
// ikinci bir üyelik sorgusu yok — olsaydı, o denetimin org ağacının dışında
// bir kopyası doğardı.
const (
	// wsWriteTimeout, tek bir mesajın yazılması için tanınan süre.
	//
	// Zorunlu: TCP gönderim tamponu dolu olan bir istemciye yazmak SÜRESİZ
	// bloklar. Zaman aşımı olmasaydı, ağı kopmuş tek bir tarayıcı sekmesi o
	// bağlantının goroutine'ini sonsuza kadar tutardı.
	wsWriteTimeout = 5 * time.Second

	// wsPingInterval / wsPingTimeout, canlılık yoklaması.
	//
	// Sessizce ölen bağlantılar (uyuyan dizüstü, NAT tablosundan düşen oturum,
	// araya giren proxy'nin boşta kalma sınırı) TCP seviyesinde dakikalarca
	// "açık" görünür. Periyodik ping, hem bu bağlantıları temizliyor hem de
	// bağlantıyı boşta sayıp kapatan ara katmanları engelliyor.
	wsPingInterval = 30 * time.Second
	wsPingTimeout  = 10 * time.Second

	// wsReadLimit, istemciden gelen tek bir mesajın üst sınırı. İstemcinin
	// gönderdiği tek şey küçük kontrol mesajları; sınırsız okuma tek bir
	// bağlantıyla belleği tüketmenin en ucuz yolu olurdu (decodeJSON'un
	// 64 KiB sınırıyla aynı fikir).
	wsReadLimit = 4 << 10

	// wsIncomingBuffer, okuma goroutine'i ile ana döngü arasındaki tampon.
	wsIncomingBuffer = 4

	// maxEchoMessage, tanı amaçlı echo mesajının uzunluk sınırı.
	maxEchoMessage = 200
)

// Olay türleri. Sunucunun ürettiği her olayın adı `realtime.` önekiyle
// başlıyor: 6.2'nin ürün olayları (`monitor.status_changed` gibi) taşıma
// katmanının kendi olaylarından ad düzeyinde ayrılabilsin.
const (
	eventReady = "realtime.ready"
	eventPong  = "realtime.pong"
	eventEcho  = "realtime.echo"
	eventError = "realtime.error"
)

// codeTicketInvalid, geçersiz/kullanılmış/süresi dolmuş bilet.
const codeTicketInvalid = "invalid_ticket"

// RealtimeNotifier, olayı süreçler arası taşıya veren bağımlılık
// (internal/realtime → Notifier).
type RealtimeNotifier interface {
	Publish(ctx context.Context, channel string, event realtime.Event) error
}

// Broadcaster, bir değişikliği ilgili kanallara duyuran bağımlılık
// (internal/realtime → Broadcaster, 6.2).
//
// api yalnızca MANUEL hareketleri duyuruyor (incident açma/güncelleme);
// otomatik durum değişimlerinin duyurusu worker'da, `checkOne`ın içinde —
// yani durum değişiminin geçtiği tek noktada.
type Broadcaster interface {
	Changed(ctx context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID, reason string)
}

// realtimeHandler, bilet ucunu ve WebSocket bağlantısını karşılar.
type realtimeHandler struct {
	hub      *realtime.Hub
	tickets  *auth.TicketService
	notifier RealtimeNotifier
	// pages, public kanalın kapısı: slug yayımlanmış mı (bkz. 5.1).
	pages StatusPageService
	// origins, izin verilen tarayıcı origin'leri — NORMALLEŞTİRİLMİŞ hâlde ve
	// istek başına değil kurulumda bir kez hesaplanmış.
	origins map[string]struct{}
	logger  *slog.Logger
	// now, testlerin saati sabitleyebilmesi için (üretimde time.Now).
	now func() time.Time
}

func newRealtimeHandler(deps Deps) *realtimeHandler {
	origins := make(map[string]struct{}, len(deps.WebOrigins))
	for _, origin := range deps.WebOrigins {
		if normalized := strings.TrimRight(strings.TrimSpace(origin), "/"); normalized != "" {
			origins[normalized] = struct{}{}
		}
	}

	return &realtimeHandler{
		hub:      deps.Realtime,
		tickets:  deps.Tickets,
		notifier: deps.Notifier,
		pages:    deps.StatusPages,
		origins:  origins,
		logger:   deps.Logger,
		now:      time.Now,
	}
}

// ticketResponse, bilet ucunun cevabı.
type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int64  `json:"expiresIn"`
}

// issueTicket — POST /v1/orgs/{orgID}/realtime-ticket
//
// Yetki ÜYELİK (member yeterli): canlı yayın panonun kendisi kadar
// salt-okunur bir yüzey ve `member` 0.1 §2'de "salt-okunur pano" olarak
// tanımlı. Yayının içeriğini bilet değil KANAL belirliyor.
func (h *realtimeHandler) issueTicket(w http.ResponseWriter, r *http.Request) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("bilet ucu üyelik doğrulaması olmadan bağlanmış",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		h.logger.Error("bilet ucu kimlik doğrulaması olmadan bağlanmış",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	ticket, err := h.tickets.Issue(identity.UserID, membership.OrgID)
	if err != nil {
		writeInternalError(w, r, h.logger, "websocket bileti üretilemedi", err)
		return
	}

	// 201 DEĞİL 200: kalıcı bir kaynak yaratılmıyor, imzalı bir dize
	// üretiliyor. Cevapta Location verilecek bir adres de yok.
	writeJSON(w, http.StatusOK, ticketResponse{
		Ticket:    ticket.Value,
		ExpiresIn: ticket.ExpiresIn,
	})
}

// connect — GET /v1/ws?ticket=…
func (h *realtimeHandler) connect(w http.ResponseWriter, r *http.Request) {
	// ORIGIN DENETİMİ İLK: bilet doğrulaması onu YAKIYOR, yani sonraya
	// bırakılsaydı reddedilen bir el sıkışma kullanıcının biletini boşa
	// harcardı.
	if !h.originAllowed(r) {
		h.logger.Warn("izin verilmeyen origin'den websocket el sıkışması",
			slog.String("origin", r.Header.Get("Origin")))
		writeError(w, r, http.StatusForbidden, codeForbidden, "bu origin'e izin verilmiyor")

		return
	}

	raw := r.URL.Query().Get("ticket")
	if raw == "" {
		writeUnauthorized(w, r, codeUnauthorized, "bilet gerekli")
		return
	}

	identity, err := h.tickets.Verify(raw)
	if err != nil {
		// ÜÇ SEBEP DE AYNI CEVABI ALIYOR (geçersiz · süresi dolmuş ·
		// kullanılmış): istemcinin yapacağı şey her üçünde de aynı — yeni bir
		// bilet al. Ayrımı LOGA yazmak, tekrar oynatma denemesini görünür
		// kılıyor; cevaba yazmak deneyen tarafa geri bildirim vermek olurdu.
		h.logger.Info("websocket bileti reddedildi",
			slog.String("error", err.Error()),
			slog.Bool("replayed", errors.Is(err, auth.ErrTicketUsed)))
		writeUnauthorized(w, r, codeTicketInvalid, "bilet geçersiz veya süresi dolmuş")

		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// KÜTÜPHANENİN ORIGIN DENETİMİ KAPALI ÇÜNKÜ YERİNE DAHA SIKISI VAR
		// (bkz. originAllowed): kütüphane yalnızca Origin'in HOST kısmını
		// istek host'uyla karşılaştırıyor, bizim listemiz ise şema+host+port
		// birlikte TAM EŞLEŞME arıyor. Varsayılan davranış bu kurulumda zaten
		// çalışmazdı: arayüz :3100, api :3400 — host'lar farklı.
		InsecureSkipVerify: true,
		// Sıkıştırma kapalı (kütüphane varsayılanı): olaylar birkaç yüz bayt,
		// sıkıştırma bağlam durumu her bağlantı için ek bellek demek.
	})
	if err != nil {
		// Accept başarısızsa cevap ZATEN yazıldı (kütüphane 400 döndü);
		// buraya ikinci bir gövde yazmak "superfluous WriteHeader" olurdu.
		h.logger.Info("websocket el sıkışması başarısız",
			slog.String("error", err.Error()))

		return
	}

	// CloseNow: düzgün kapanış (kapanış çerçevesi + karşı tarafı bekleme) ana
	// döngünün işi; buradaki savunma, o yol hiç çalışmazsa soketin ve okuma
	// goroutine'inin ASILI KALMAMASI için.
	defer func() { _ = conn.CloseNow() }()

	conn.SetReadLimit(wsReadLimit)

	h.serve(r.Context(), conn, identity)
}

// connectPublic — GET /v1/public/ws?slug=…
//
// OTURUMSUZ (6.2). 5.1'in public alt-router'ında duruyor ve bilinçli olarak
// `requireAuth` ağacının dışında: kimliği yoldaki SLUG taşıyor, tıpkı okuma
// ucunda olduğu gibi.
//
// ORIGIN DENETİMİ YOK — ve bu bir eksiklik değil, kararın kendisi. Origin
// denetimi WebSocket'te CSWSH'ye karşıdır: saldırgan bir sitenin, kullanıcının
// AMBIENT kimlik bilgisiyle (cookie) sokete bağlanmasını engeller. Burada
// ortada kimlik yok — kanal herkese açık public veriyi yayıyor ve bağlanmak
// için sayfanın slug'ını bilmek yeterli. Üstelik denetim yazılsaydı 5.4'ün
// ALT ALAN ADI erişimini (`acme.localhost:3100`) kırardı: public bir status
// sayfası tam da her yerden ulaşılabilir olmak için var.
func (h *realtimeHandler) connectPublic(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	if slug == "" {
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir status sayfası yok")
		return
	}

	// YAYIMLANMAMIŞ ≡ OLMAYAN (5.1): kapı okuma ucunun kullandığı sorgunun
	// aynısı. Yayımlanmamış bir sayfanın kanalını açık tutmak, "sayfayı
	// okuyamayan ama değişikliklerini duyabilen ziyaretçi" demek olurdu.
	if err := h.pages.PublishedSlug(r.Context(), slug); err != nil {
		if errors.Is(err, statuspage.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir status sayfası yok")
			return
		}

		writeInternalError(w, r, h.logger, "status sayfası okunamadı", err)

		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.logger.Info("public websocket el sıkışması başarısız",
			slog.String("error", err.Error()))

		return
	}

	defer func() { _ = conn.CloseNow() }()

	conn.SetReadLimit(wsReadLimit)

	h.servePublic(r.Context(), conn, slug)
}

// servePublic, oturumsuz bağlantının ömrünü yönetir.
//
// Oturumlu `serve` ile aynı iskelet ama İSTEMCİDEN MESAJ KABUL ETMİYOR:
// ziyaretçinin sunucuya söyleyeceği hiçbir şey yok. Okuma yine de yapılıyor —
// kapanmayı ve ölü bağlantıyı fark etmenin tek yolu o.
func (h *realtimeHandler) servePublic(reqCtx context.Context, conn *websocket.Conn, slug string) {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()

	channel := realtime.PublicChannel(slug)

	sub := h.hub.Subscribe(channel)
	defer sub.Close()

	incoming := make(chan clientMessage, wsIncomingBuffer)
	go h.readPump(ctx, cancel, conn, incoming)

	h.logger.Info("public websocket bağlantısı açıldı",
		slog.String("slug", slug),
		slog.Int("subscribers", h.hub.Subscribers(channel)))

	ready, err := realtime.NewEvent(eventReady, h.now(), map[string]string{"slug": slug})
	if err != nil || h.write(ctx, conn, ready) != nil {
		return
	}

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")

			return

		case <-sub.Dropped():
			h.closePublicDropped(conn, sub.DropReason(), slug)

			return

		case event := <-sub.Events():
			if err := h.write(ctx, conn, event); err != nil {
				return
			}

		case <-incoming:
			// Gelen mesaj SESSİZCE yok sayılıyor: bu kanalda istemcinin bir
			// sözleşmesi yok. Hata olayı döndürmek, oturumsuz bir yüzeye
			// gereksiz bir konuşma yüzeyi eklemek olurdu.

		case <-ping.C:
			if err := h.ping(ctx, conn); err != nil {
				return
			}
		}
	}
}

func (h *realtimeHandler) closePublicDropped(conn *websocket.Conn, reason realtime.DropReason, slug string) {
	status := websocket.StatusTryAgainLater
	if reason == realtime.DropShutdown {
		status = websocket.StatusGoingAway
	}

	h.logger.Info("public websocket aboneliği düşürüldü",
		slog.String("slug", slug),
		slog.String("reason", string(reason)))

	_ = conn.Close(status, string(reason))
}

// serve, tek bir bağlantının ömrünü yönetir.
//
// GOROUTINE SÖZLEŞMESİ: bağlantı başına İKİ goroutine var — bu (yazma) ve
// readPump (okuma). Kütüphane eşzamanlı okumayı desteklemediği için okuma ayrı
// bir goroutine'de; ikisi de aynı context'e bağlı ve hangisi önce biterse
// context iptal edilip diğeri de çıkıyor. Yani kapanan bir bağlantı arkasında
// goroutine bırakmıyor.
func (h *realtimeHandler) serve(reqCtx context.Context, conn *websocket.Conn, identity auth.TicketIdentity) {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()

	channel := realtime.OrgChannel(identity.OrgID)

	// ABONELİK KANALI BİLETTEN: istemcinin gönderdiği hiçbir değer kanal adına
	// girmiyor. Bir org'un olayını başka bir org'un görmesi için sunucunun
	// kanal adını yanlış üretmesi gerekir.
	sub := h.hub.Subscribe(channel)
	defer sub.Close()

	incoming := make(chan clientMessage, wsIncomingBuffer)
	go h.readPump(ctx, cancel, conn, incoming)

	h.logger.Info("websocket bağlantısı açıldı",
		slog.String("org_id", identity.OrgID.String()),
		slog.String("user_id", identity.UserID.String()),
		slog.Int("subscribers", h.hub.Subscribers(channel)))

	// İLK MESAJ BAĞLANTININ KURULDUĞUNU SÖYLÜYOR: WebSocket el sıkışması
	// istemcide `onopen` ile görülüyor ama bu yalnızca "soket açıldı" demek.
	// Sunucunun abone listesine gerçekten girdiğini söyleyen tek şey bu olay —
	// istemci "kaçırdığım var mı" tazelemesini (6.2) buna bağlayabilsin.
	ready, err := realtime.NewEvent(eventReady, h.now(), map[string]string{
		"orgId": identity.OrgID.String(),
	})
	if err != nil || h.write(ctx, conn, ready) != nil {
		return
	}

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			// İstek context'i: istemci gitti ya da süreç kapanıyor.
			_ = conn.Close(websocket.StatusNormalClosure, "")

			return

		case <-sub.Dropped():
			h.closeDropped(conn, sub.DropReason(), identity)

			return

		case event := <-sub.Events():
			if err := h.write(ctx, conn, event); err != nil {
				h.logger.Info("websocket yazma başarısız, bağlantı kapatılıyor",
					slog.String("org_id", identity.OrgID.String()),
					slog.String("event", event.Type),
					slog.String("error", err.Error()))

				return
			}

		case msg := <-incoming:
			h.handleClientMessage(ctx, conn, identity, msg)

		case <-ping.C:
			if err := h.ping(ctx, conn); err != nil {
				h.logger.Info("websocket canlılık yoklaması başarısız",
					slog.String("org_id", identity.OrgID.String()),
					slog.String("error", err.Error()))

				return
			}
		}
	}
}

// closeDropped, hub tarafından düşürülen aboneliğin bağlantısını kapatır.
func (h *realtimeHandler) closeDropped(
	conn *websocket.Conn, reason realtime.DropReason, identity auth.TicketIdentity,
) {
	// 1013 (Try Again Later): istemciye "sende bir sorun yok, yeniden bağlan"
	// demenin standart yolu. 1000 (normal) deseydik istemci bunu kasıtlı bir
	// kapanış sayıp yeniden bağlanmayabilirdi.
	status := websocket.StatusTryAgainLater
	if reason == realtime.DropShutdown {
		// 1001 (Going Away): sunucu kapanıyor.
		status = websocket.StatusGoingAway
	}

	h.logger.Info("websocket aboneliği düşürüldü",
		slog.String("org_id", identity.OrgID.String()),
		slog.String("reason", string(reason)))

	_ = conn.Close(status, string(reason))
}

// clientMessage, istemciden gelen kontrol mesajı.
type clientMessage struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// readPump, istemciden gelen mesajları okur.
//
// Ayrı bir goroutine olmasının sebebi kütüphanenin sözleşmesi: `Read` aynı
// anda tek bir goroutine'den çağrılabiliyor ve bloklayan bir çağrı. Çıkarken
// context'i iptal ediyor — istemci bağlantıyı kapattığında yazma tarafı da
// hemen çıksın (yoksa bunu ancak bir sonraki olay veya ping'de fark ederdi).
func (h *realtimeHandler) readPump(
	ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, out chan<- clientMessage,
) {
	defer cancel()

	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		if msgType != websocket.MessageText {
			continue
		}

		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			// Bozuk mesaj bağlantıyı DÜŞÜRMÜYOR: istemcinin bir hatası tüm
			// canlı akışını kaybetmesine yol açmasın.
			h.logger.Debug("çözümlenemeyen websocket mesajı atlandı",
				slog.String("error", err.Error()))

			continue
		}

		select {
		case out <- msg:
		case <-ctx.Done():
			return
		}
	}
}

// handleClientMessage, istemcinin gönderdiği kontrol mesajını karşılar.
func (h *realtimeHandler) handleClientMessage(
	ctx context.Context, conn *websocket.Conn, identity auth.TicketIdentity, msg clientMessage,
) {
	switch msg.Type {
	case "ping":
		// PING DOĞRUDAN CEVAPLANIYOR, taşıma yoluna girmeden: sorulan şey "bu
		// BAĞLANTI çalışıyor mu". Cevabı org kanalından geçirmek, aynı org'un
		// diğer sekmelerine de bir "pong" göndermek olurdu.
		pong, err := realtime.NewEvent(eventPong, h.now(), nil)
		if err == nil {
			_ = h.write(ctx, conn, pong)
		}

	case "echo":
		h.handleEcho(ctx, conn, identity, msg)

	default:
		h.writeError(ctx, conn, "bilinmeyen mesaj türü")
	}
}

// handleEcho, tanı amaçlı echo mesajını TAŞIMA YOLUNUN TAMAMINDAN geçirir.
//
// Bu, 6.1'in uçtan uca kanıtı: mesaj api'den `pg_notify` ile Postgres'e
// gidiyor, dinleyici onu geri alıyor, Hub org kanalındaki TÜM bağlantılara
// dağıtıyor. Yani aynı org'un ikinci bir sekmesi de görüyor, başka bir org
// görmüyor — hem taşıma hem izolasyon tek hareketle doğrulanabiliyor.
//
// Yayın org'un KENDİ kanalıyla sınırlı ve gövde 200 karakterle kapalı; yetki
// olarak org üyeliği yeterli çünkü mesaj org'un dışına çıkmıyor.
func (h *realtimeHandler) handleEcho(
	ctx context.Context, conn *websocket.Conn, identity auth.TicketIdentity, msg clientMessage,
) {
	if len(msg.Message) > maxEchoMessage {
		h.writeError(ctx, conn, "echo mesajı çok uzun")
		return
	}

	event, err := realtime.NewEvent(eventEcho, h.now(), map[string]string{
		"message": msg.Message,
		"from":    identity.UserID.String(),
	})
	if err != nil {
		h.writeError(ctx, conn, "olay üretilemedi")
		return
	}

	// Yayın için ayrı bir zaman sınırı: veritabanı yavaşsa bağlantının ana
	// döngüsü burada takılıp gelen olayları geciktirmesin.
	publishCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	if err := h.notifier.Publish(publishCtx, realtime.OrgChannel(identity.OrgID), event); err != nil {
		h.logger.Warn("echo olayı yayımlanamadı",
			slog.String("org_id", identity.OrgID.String()),
			slog.String("error", err.Error()))
		h.writeError(ctx, conn, "olay yayımlanamadı")
	}
}

// write, tek bir olayı YAZMA ZAMAN AŞIMIYLA gönderir.
func (h *realtimeHandler) write(ctx context.Context, conn *websocket.Conn, event realtime.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	return conn.Write(writeCtx, websocket.MessageText, payload)
}

// writeError, istemciye hata olayı gönderir (bağlantıyı kapatmadan).
func (h *realtimeHandler) writeError(ctx context.Context, conn *websocket.Conn, message string) {
	event, err := realtime.NewEvent(eventError, h.now(), map[string]string{"message": message})
	if err != nil {
		return
	}

	_ = h.write(ctx, conn, event)
}

// ping, canlılık yoklaması yapar ve pong bekler.
func (h *realtimeHandler) ping(ctx context.Context, conn *websocket.Conn) error {
	pingCtx, cancel := context.WithTimeout(ctx, wsPingTimeout)
	defer cancel()

	return conn.Ping(pingCtx)
}

// originAllowed, el sıkışmanın Origin başlığını ELLE denetler.
//
// NEDEN ELLE: CORS WebSocket'e UYGULANMIYOR. Tarayıcı el sıkışmayı same-origin
// politikasına tabi tutmuyor, preflight göndermiyor ve sunucunun CORS
// başlıklarına bakmıyor — yani corsMiddleware'in listesi bu yola hiç girmiyor
// (5.1'in "public sayfa CORS yoluna girmiyor" kararı SSR fetch'i içindi;
// WebSocket ayrı bir yol ve tarayıcıdan açılıyor). Denetim yazılmazsa
// herhangi bir sitedeki script bu sokete bağlanmayı deneyebilir.
//
// Origin'i OLMAYAN istek kabul ediliyor: tarayıcı dışı istemciler (curl,
// wscat, Go testleri) bu başlığı göndermiyor ve göndermeye de mecbur değiller
// — zaten istedikleri değeri yazabilirlerdi. Origin denetimi tarayıcıda
// çalışan SİTELERİ birbirinden ayırmak için var; gerçek kimlik doğrulaması
// bilet.
func (h *realtimeHandler) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	_, ok := h.origins[strings.TrimRight(origin, "/")]

	return ok
}
