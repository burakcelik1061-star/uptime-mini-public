// Package httpapi, api sürecinin HTTP yüzeyini kurar.
//
// Router burada, main.go'da değil: Faz 7.1'in integration testleri gerçek
// router'ı (tüm middleware zinciriyle birlikte) ayağa kaldırıp test edecek.
// canny-mini'nin dersi buydu — testlerin kendi "uygulama kurulumu" kopyasını
// tutması, prod'da olan bir middleware'in testlerde sessizce eksik kalmasına
// yol açıyor.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// requestTimeout, normal bir HTTP isteğinin üst sınırı.
//
// WEBSOCKET UCU BU SINIRIN DIŞINDA (6.1): `middleware.Timeout` istek
// context'ini iptal ediyor ve süre dolduğunda cevaba 504 yazmaya çalışıyor —
// saatlerce açık kalan, üstelik hijack edilmiş bir bağlantı için ikisi de
// yanlış. Kural bir istisna ile delinmiyor: /v1/ws bu middleware'i taşıyan
// grubun DIŞINDA duruyor (bkz. NewRouter).
const requestTimeout = 30 * time.Second

// Deps, router'ın ihtiyaç duyduğu bağımlılıklar.
type Deps struct {
	Logger    *slog.Logger
	StartedAt time.Time
	// DB, hazır olma kontrolü için. Somut havuz değil arayüz: testler
	// gerçek bir veritabanı olmadan iki yolu da ölçebiliyor.
	DB Pinger
	// Users, auth uçlarının veri erişimi (bkz. auth.go → UserStore).
	Users UserStore
	// Hasher ve Tokens, parola ve access token işleri.
	Hasher auth.Hasher
	Tokens *auth.TokenService
	// Sessions, refresh token oturumları (bkz. session.go → SessionService).
	Sessions SessionService
	// Orgs, organizasyon işlemleri (bkz. orgs.go → OrgService).
	Orgs OrgService
	// Invitations, organizasyon davetleri (8.3).
	Invitations InvitationService
	// PasswordReset, parola sıfırlama akışı (8.4).
	PasswordReset PasswordResetService
	// Plans, plan & kota (8.5).
	Plans PlanService
	// PublicBaseURL, e-postadaki linklerin kökü — ARAYÜZÜN adresi (6.4/8.3).
	PublicBaseURL string
	// Monitors, monitor CRUD'u (bkz. monitors.go → MonitorService).
	Monitors MonitorService
	// Checks, manuel "şimdi kontrol et" (bkz. checks.go → CheckRunner).
	Checks CheckRunner
	// Results, kontrol sonuçlarının zaman serisi (bkz. internal/results).
	Results CheckResultService
	// Incidents, kesinti kayıtları ve timeline (bkz. internal/incidents).
	Incidents IncidentService
	// Maintenance, planlı bakım pencereleri (bkz. internal/maintenance).
	Maintenance MaintenanceService
	// StatusPages, public status page'in hem admin hem ziyaretçi yüzü
	// (bkz. internal/statuspage).
	StatusPages StatusPageService
	// Notifications, bildirim kanalları (bkz. internal/notifications, 6.3).
	Notifications NotificationService
	// Subscribers, status page e-posta abonelikleri (6.4) — OTURUMSUZ yüzey.
	Subscribers SubscriberService
	// Outbox, mock e-posta adapter'ının posta kutusu (8.2). NIL = geliştirici
	// posta kutusu KAPALI ve ucu router'a hiç kaydedilmiyor. Kapının kararı
	// burada değil config'de veriliyor (APP_ENV açıkça development|test).
	// SOMUT İŞARETÇİ, arayüz değil — gerekçesi outbox.go'da (tipli-nil tuzağı).
	Outbox *notifications.Outbox
	// Realtime, açık WebSocket bağlantılarının kanal kaydı (6.1).
	Realtime *realtime.Hub
	// Tickets, WebSocket el sıkışmasının kısa ömürlü biletleri (6.1).
	Tickets *auth.TicketService
	// Notifier, olayı süreçler arası taşıya veren yayıncı (6.1).
	Notifier RealtimeNotifier
	// Broadcaster, manuel incident hareketlerini canlı yayına duyurur (6.2).
	Broadcaster Broadcaster
	// WebOrigins, tarayıcı arayüzüne izin verilen origin'ler (CORS ve —
	// AYRICA — WebSocket el sıkışmasının Origin denetimi, bkz. ws.go).
	WebOrigins []string
	// SecureCookies, cookie'lere Secure bayrağı konsun mu (üretimde evet).
	SecureCookies bool
	// TrustedProxies, `X-Forwarded-For` başlığına güvenilecek KAYNAK adresler
	// (IP veya CIDR). BOŞ = hiçbir başlık okunmuyor, kimlik ham TCP adresi
	// (0.4'ün `middleware.RealIP` kararı). Bkz. ratelimit.go.
	TrustedProxies []string
}

// NewRouter, uygulamanın tüm HTTP rotalarını ve middleware zincirini kurar.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	// HIZ SINIRLARI (7.4) — her yüzeyin KENDİ kovası. Tek bir paylaşılan kova,
	// giriş denemelerinin abonelik ucunu da kilitlemesi (ve tersi) demekti;
	// sınırların büyüklüğü de yüzeye göre farklı (bkz. ratelimit.go).
	authLimiter := newRateLimiter(authRateBurst, authRatePeriod)
	subscribeLimiter := newRateLimiter(subscribeRateBurst, subscribeRatePeriod)
	// Onay/iptal linkleri ABONELİKTEN AYRI: iptal e-postadaki tek çıkış yolu
	// ve bir abonelik seliyle bloklanmamalı (7.5'in bulgusu).
	subscriptionLinkLimiter := newRateLimiter(subscriptionLinkRateBurst, subscriptionLinkRatePeriod)
	realtimeLimiter := newRateLimiter(realtimeRateBurst, realtimeRatePeriod)
	// Public OKUMA ucu: oturumsuz yüzeylerin en pahalısı ve 7.4'te atlanmıştı.
	publicReadLimiter := newRateLimiter(publicReadRateBurst, publicReadRatePeriod)

	// İSTEMCİ KİMLİĞİ: varsayılan ham TCP adresi. `TRUSTED_PROXIES` doluysa o
	// kaynaklardan gelen `X-Forwarded-For` okunuyor — public yüzeyler Next
	// sunucusundan çağrıldığı için gerçek ziyaretçi adresi yalnızca orada
	// (bkz. ratelimit.go'nun paket yorumu).
	clients := newClientIdentifier(deps.TrustedProxies)

	// RequestID: her isteğin izlenebilir bir kimliği olsun (Faz 7.4'ün hata
	// izleme hikayesinin temeli).
	r.Use(middleware.RequestID)
	//
	// `middleware.RealIP` BİLİNÇLİ OLARAK KULLANILMIYOR: `X-Forwarded-For`
	// başlığını doğrulamadan `RemoteAddr`'ın yerine yazıyor, yani herhangi
	// bir istemci kendi IP'sini uydurabiliyor (chi bu yüzden kullanımdan
	// kaldırdı). Rate limiting (Faz 7.4) istemci IP'sine dayanacağı için
	// sahte bir IP doğrudan koruma atlatması demek olurdu. Proxy arkasında
	// çalışmak gerekirse orada, GÜVENİLEN proxy listesiyle birlikte
	// çözülecek — canny-mini'deki `TRUST_PROXY` kararının aynısı.
	r.Use(requestLogger(deps.Logger))
	// Recoverer: bir handler panikleyince süreç ÖLMESİN, sadece o istek
	// 500 alsın. Go'da panik varsayılan olarak tüm süreci düşürür.
	r.Use(middleware.Recoverer)
	// CORS, Recoverer'dan SONRA: preflight cevabı da panik korumasının
	// içinde kalsın. Loglayıcıdan sonra olması da bilinçli — reddedilen
	// preflight istekleri de erişim logunda görünüyor.
	r.Use(corsMiddleware(deps.WebOrigins, deps.Logger))

	// chi'nin düz metin cevapları yerine standart hata zarfı.
	r.NotFound(notFoundHandler())
	r.MethodNotAllowed(methodNotAllowedHandler())

	// İki ayrı uç, iki ayrı soru: /health "süreç ayakta mı" (liveness),
	// /ready "istek karşılayabilir mi" (readiness, veritabanını yoklar).
	//
	// Bu ikisi /v1 ALTINDA DEĞİL: altyapı uçları, ürün API'sinin bir parçası
	// değil. Compose healthcheck'i ve orkestratör, API sürümlendiğinde
	// (v2) yol değiştirmek zorunda kalmamalı.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(requestTimeout))

		r.Get("/health", healthHandler(deps.StartedAt))
		r.Get("/ready", readyHandler(deps.DB))
	})

	handler := &authHandler{
		users:         deps.Users,
		hasher:        deps.Hasher,
		tokens:        deps.Tokens,
		sessions:      deps.Sessions,
		logger:        deps.Logger,
		secureCookies: deps.SecureCookies,
	}

	organizations := &orgHandler{orgs: deps.Orgs, logger: deps.Logger}
	planEndpoints := &planHandler{plans: deps.Plans, logger: deps.Logger}
	passwordResetEndpoints := &passwordResetHandler{
		reset:  deps.PasswordReset,
		logger: deps.Logger,
	}
	invitationEndpoints := &invitationHandler{
		invitations: deps.Invitations,
		users:       deps.Users,
		// Davet linkinin kökü ARAYÜZÜN adresi, api'nin değil (6.4'ün kararı):
		// tıklayan kişi bir JSON cevabı değil, ne olduğunu anlatan bir sayfa
		// görmeli.
		publicBaseURL: deps.PublicBaseURL,
		logger:        deps.Logger,
	}
	incidentEndpoints := &incidentHandler{
		incidents:   deps.Incidents,
		broadcaster: deps.Broadcaster,
		logger:      deps.Logger,
	}
	maintenanceEndpoints := &maintenanceHandler{windows: deps.Maintenance, logger: deps.Logger}
	statusPageEndpoints := &statusPageHandler{pages: deps.StatusPages, logger: deps.Logger}
	publicEndpoints := &publicHandler{pages: deps.StatusPages, logger: deps.Logger}
	notificationEndpoints := &notificationHandler{channels: deps.Notifications, logger: deps.Logger}
	subscriberEndpoints := &subscriberHandler{subscribers: deps.Subscribers, logger: deps.Logger}
	realtimeEndpoints := newRealtimeHandler(deps)
	monitorEndpoints := &monitorHandler{
		monitors: deps.Monitors,
		checks:   deps.Checks,
		results:  deps.Results,
		logger:   deps.Logger,
	}

	// /v1: dışa verilen sözleşmenin sürümü. Kırıcı bir değişiklik
	// gerektiğinde eski istemciler çalışmaya devam edebilsin diye baştan
	// sürümlü başlıyoruz — sonradan sürüm eklemek, sürümsüz yolları sonsuza
	// kadar taşımak demek.
	r.Route("/v1", func(r chi.Router) {
		// WEBSOCKET EL SIKIŞMASI (6.1) — İSTEK ZAMAN AŞIMININ DIŞINDA ve
		// bilinçli olarak `requireAuth` ağacının da dışında: tarayıcının
		// WebSocket API'si `Authorization` başlığı gönderemiyor, kimlik
		// sorgudaki KISA ÖMÜRLÜ BİLET ile taşınıyor (bkz. ws.go). Bileti veren
		// uç ise org-scoped ağacın içinde — üyelik orada, veritabanına karşı
		// doğrulanıyor.
		r.With(rateLimit(realtimeLimiter, clients, deps.Logger)).
			Get("/ws", realtimeEndpoints.connect)

		// PUBLIC YÜZEY (5.1) — oturum İSTEMEYEN tek alt-router ve bilinçli
		// olarak aşağıdaki korumalı grubun DIŞINDA.
		//
		// Kimliği yoldaki SLUG taşıyor: ne token var ne üyelik, dolayısıyla
		// `requireOrgMembership`in koruduğu ağacın "buraya gelen her istek
		// doğrulanmış" varsayımı burada geçerli değil. Ucu o ağaca bağlayıp
		// istisna yazmak, varsayımı tüm ağaç için zayıflatmak olurdu (1.4).
		// Org verisine erişimi kendi sorguları sınırlıyor: yayımlanmamış bir
		// servisin adı hiçbir alandan çıkmıyor (bkz. public.go).
		//
		// 6.2'nin CANLI KANALI DA BURADA — korumalı ağaçta değil. Blok bu
		// yüzden zaman aşımı satırının ÜSTÜNDE: WebSocket ucu sınırın dışında
		// kalmalı, okuma ucu ise normal bir istek olarak içinde.
		r.Route("/public", func(r chi.Router) {
			r.With(rateLimit(realtimeLimiter, clients, deps.Logger)).
				Get("/ws", realtimeEndpoints.connectPublic)

			r.Group(func(r chi.Router) {
				r.Use(middleware.Timeout(requestTimeout))

				// HIZ SINIRI (7.5): bu uç istek başına on bir sorgu çalıştırıyor
				// (90 günlük özet taraması dahil) ve kimlik istemiyor —
				// oturumsuz yüzeylerin en pahalısı. Kotası geniş, gerekçesi
				// ratelimit.go'da (önbellek yerine sınır seçilme sebebi dahil).
				r.With(rateLimit(publicReadLimiter, clients, deps.Logger)).
					Get("/status/{slug}", publicEndpoints.status)

				// ABONELİK (6.4) — ziyaretçinin YAZDIĞI tek yüzey ve 0.1 §2'nin
				// "ziyaretçi hiçbir şey yazmaz" kuralının bilinçli istisnası.
				// Kimlik yok: abone bir `User` değil, kimliğini onay ve iptal
				// linklerindeki tahmin edilemez token'lar taşıyor.
				//
				// HIZ SINIRI (7.4): abonelik ucu, E-POSTA GÖNDERİMİNİ
				// TETİKLEYEN TEK OTURUMSUZ UÇ — dar kova.
				r.With(rateLimit(subscribeLimiter, clients, deps.Logger)).
					Post("/status/{slug}/subscribers", subscriberEndpoints.subscribe)

				// ONAY VE İPTAL AYRI KOVADA (7.5'in bulgusu). 7.4 üçünü tek
				// kovada toplamıştı; gerekçesi doğruydu (ikisi de tahmin
				// edilemez token doğruluyor, sınırsız deneme ücretsiz bir
				// tarama yüzeyi olurdu) ama sonucu yanlıştı: abonelik ucuna
				// yapılan bir sel e-postadaki İPTAL LİNKİNİ de düşürüyordu.
				// İptal, aboneliğin tek çıkış yolu — başkasının "abone ol"
				// denemeleri onu bloklayamaz.
				r.Group(func(r chi.Router) {
					r.Use(rateLimit(subscriptionLinkLimiter, clients, deps.Logger))

					r.Post("/subscriptions/confirm", subscriberEndpoints.confirm)
					r.Post("/subscriptions/unsubscribe", subscriberEndpoints.unsubscribe)
				})
			})
		})

		// Buradan sonraki HER uç istek zaman aşımını taşıyor. Yeniden atama
		// bilinçli: aşağıdaki hiçbir uç bu sınırın dışına, yukarıdaki
		// WebSocket ucu ise içine düşmesin.
		r = r.With(middleware.Timeout(requestTimeout))

		// HIZ SINIRI (7.4): auth ağacının TAMAMI oturumsuz ve parola
		// denemesi/hesap açma buradan geçiyor. `refresh` de dahil —
		// 1.2'nin "yeniden kullanılan token AİLEYİ iptal eder" kuralı,
		// sınırsız deneme ile bir kullanıcının oturumunu düşürme aracına
		// dönüşebilirdi.
		r.Group(func(r chi.Router) {
			r.Use(rateLimit(authLimiter, clients, deps.Logger))

			r.Post("/auth/register", handler.register)
			r.Post("/auth/login", handler.login)
			// refresh ve logout access token İSTEMEZ: kimliklerini httpOnly
			// cookie'deki refresh token'dan alıyorlar. (Zaten access token'ın
			// süresi dolduğunda çağrılıyorlar.)
			r.Post("/auth/refresh", handler.refresh)
			r.Post("/auth/logout", handler.logout)

			// PAROLA SIFIRLAMA (8.4) — oturumsuz, aynı hız sınırı kovasında.
			// İstek ucu e-posta gönderimini tetikleyen İKİNCİ oturumsuz uç
			// (birincisi 6.4'ün aboneliği).
			r.Post("/auth/password-reset", passwordResetEndpoints.request)
			r.Post("/auth/password-reset/confirm", passwordResetEndpoints.confirm)
		})

		// Korumalı rotalar TEK BİR grupta: middleware rotaya tek tek
		// takılsaydı, yeni bir uç eklerken unutulması sessiz bir açık olurdu.
		r.Group(func(r chi.Router) {
			r.Use(requireAuth(deps.Tokens))

			r.Get("/me", handler.me)

			// GELİŞTİRİCİ POSTA KUTUSU (8.2) — KOŞULLU KAYIT.
			//
			// `Deps.Outbox` yalnızca APP_ENV açıkça development|test iken
			// doluyor (config.devToolsEnabled → cmd/api/main.go); nil ise uç
			// ROUTER'A HİÇ GİRMİYOR ve istek standart 404 zarfını alıyor.
			// Handler'ın içinde bir ortam kontrolü yazmak, korumayı
			// "unutulabilir" sınıfına sokardı (1.4'ün kararı).
			//
			// Uç ORG-SCOPED DEĞİL: mesajların hepsi bir organizasyona ait
			// değil (bkz. outbox.go). Bu yüzden aşağıdaki `/orgs/{orgID}`
			// ağacının değil, oturumlu grubun doğrudan altında.
			if deps.Outbox != nil {
				r.Get("/dev/outbox", (&outboxHandler{
					outbox: deps.Outbox,
					logger: deps.Logger,
				}).list)
			}

			r.Post("/orgs", organizations.create)
			r.Get("/orgs", organizations.list)

			// DAVETİ KABUL ETME (8.3) — ORG-SCOPED AĞACIN DIŞINDA, bilinçli.
			//
			// Kabul eden kişi henüz ÜYE DEĞİL: `requireOrgMembership` ona 404
			// döndürürdü. Ucu o ağaca bağlayıp "davet varsa geç" istisnası
			// yazmak, 1.4'ün "kontrolü atlamak için ağacın dışına çıkmak
			// gerekir" garantisini TÜM uçlar için zayıflatırdı (5.1'in public
			// yüzeyi ayrı alt-router'a almasının aynı gerekçesi). Kimliği
			// yoldaki org değil, gövdedeki TOKEN taşıyor.
			r.Post("/invitations/accept", invitationEndpoints.accept)

			// Org-scoped alt-router: buradan sonraki HER uç, üyeliği
			// veritabanından doğrulanmış bir istek görüyor (bkz.
			// orgscope.go). Faz 2'den itibaren monitor/incident uçları da
			// bu ağacın altına eklenecek.
			r.Route("/orgs/{orgID}", func(r chi.Router) {
				r.Use(requireOrgMembership(deps.Orgs, deps.Logger))

				r.Get("/", organizations.detail)
				r.Get("/members", organizations.members)

				// PLAN & KOTA (8.5).
				r.Route("/plan", func(r chi.Router) {
					// OKUMA üyeye açık: "hangi plandayız, kotanın
					// neresindeyiz" salt-okunur panonun parçası (0.1 §2).
					r.Get("/", planEndpoints.get)

					// DEĞİŞTİRME OWNER'A ÖZEL, admin'e değil: plan
					// organizasyonun faturasını belirliyor ve 0.1 §2'nin
					// yetki tablosunda admin'in yapamadığı şeyler (org
					// silme, ownership devri) aynı sınıfta.
					r.With(requireOrgRole(store.OrgRoleOwner, deps.Logger)).
						Put("/", planEndpoints.change)
				})

				// DAVET YÖNETİMİ (8.3) — org-scoped ağacın İÇİNDE, yani
				// 7.1'in `chi.Walk` testi bu üç ucu kendiliğinden kapsıyor.
				r.Route("/invitations", func(r chi.Router) {
					// OKUMA ÜYEYE AÇIK: "kim davet edilmiş" salt-okunur
					// panonun parçası (0.1 §2), üye listesi gibi.
					r.Get("/", invitationEndpoints.list)

					// YAZMA: admin+ (0.1 §2'nin yetki tablosunda "üye
					// yönetimi" admin'e ait). Kimin organizasyona
					// girebileceğine karar vermek yönetim kararı; yetki
					// YÜKSELTME kapısı ayrıca serviste (davet eden kendi
					// rolünün üstünde rol veremiyor).
					r.Group(func(r chi.Router) {
						r.Use(requireOrgRole(store.OrgRoleAdmin, deps.Logger))

						r.Post("/", invitationEndpoints.create)
						r.Delete("/{invitationID}", invitationEndpoints.remove)
					})
				})

				// CANLI YAYIN BİLETİ (6.1): üyelik yeterli — canlı yayın
				// panonun kendisi kadar salt-okunur bir yüzey. Ucun BURADA
				// olması, biletin içine yazılan org kimliğinin
				// `requireOrgMembership` tarafından veritabanına karşı
				// doğrulanmış değer olmasını sağlıyor.
				//
				// HIZ SINIRI (7.4): bilet almak ucuz görünüyor ama her bilet
				// bir bağlantı hakkı, yani sunucuda bellek ve goroutine
				// (6.1'in hub'ı). Oturumlu ağaçtaki TEK sınırlı uç.
				r.With(rateLimit(realtimeLimiter, clients, deps.Logger)).
					Post("/realtime-ticket", realtimeEndpoints.issueTicket)

				r.Route("/monitors", func(r chi.Router) {
					// OKUMA: üyelik yeterli. `member` rolü 0.1 §2'de
					// "salt-okunur pano" olarak tanımlı.
					r.Get("/", monitorEndpoints.list)
					r.Get("/{monitorID}", monitorEndpoints.detail)
					// Kontrol GEÇMİŞİ ve ÖZETİ okuma tarafında: pano verisi.
					r.Get("/{monitorID}/checks", monitorEndpoints.listChecks)
					r.Get("/{monitorID}/summary", monitorEndpoints.summary)

					// YAZMA: admin+ (owner dahil, "en az şu rol" sıralaması).
					// Ayrı bir grupta, tek bir Use ile: yeni bir yazma ucu
					// eklerken rolü unutmak için grubun DIŞINA çıkmak gerekir.
					r.Group(func(r chi.Router) {
						r.Use(requireOrgRole(store.OrgRoleAdmin, deps.Logger))

						r.Post("/", monitorEndpoints.create)
						r.Patch("/{monitorID}", monitorEndpoints.update)
						r.Delete("/{monitorID}", monitorEndpoints.remove)

						// Manuel kontrol de YAZMA tarafında: sunucuyu
						// dışarıya istek attıran bir yan etki, salt-okunur
						// bir pano işlemi değil (bkz. checks.go).
						r.Post("/{monitorID}/check", monitorEndpoints.runCheck)
					})
				})

				r.Route("/incidents", func(r chi.Router) {
					// OKUMA: üyelik yeterli — `member` rolü 0.1 §2'de
					// "incident geçmişi dahil salt-okunur pano".
					r.Get("/", incidentEndpoints.list)
					r.Get("/{incidentID}", incidentEndpoints.detail)

					// TIMELINE'A YAZMA DA ÜYEYE AÇIK (4.2): nöbetçi mühendis
					// senaryosu — kesintiyi gören kişi ne olduğunu yazabilmeli.
					// İncident'ı KAPATMAK admin+ ve o kontrol handler'ın
					// içinde (yalnızca `status: resolved` için).
					r.Post("/{incidentID}/updates", incidentEndpoints.addUpdate)

					// MANUEL incident oluşturmak admin+ (0.1 §2: "incident
					// yönetimi (oluşturma/güncelleme/kapatma)" admin yetkisi).
					r.Group(func(r chi.Router) {
						r.Use(requireOrgRole(store.OrgRoleAdmin, deps.Logger))

						r.Post("/", incidentEndpoints.create)
					})
				})

				r.Route("/maintenance-windows", func(r chi.Router) {
					// OKUMA: üyelik yeterli — "ne zaman bakım var" salt-okunur
					// panonun parçası.
					r.Get("/", maintenanceEndpoints.list)

					// YAZMA: admin+ (0.1 §2'de "bakım penceresi tanımlama"
					// açıkça admin yetkisi). Pencere otomatik incident'ları
					// bastırıyor: yanlış kullanımı gerçek bir kesintiyi
					// görünmez yapabilir.
					r.Group(func(r chi.Router) {
						r.Use(requireOrgRole(store.OrgRoleAdmin, deps.Logger))

						r.Post("/", maintenanceEndpoints.create)
						r.Delete("/{windowID}", maintenanceEndpoints.remove)
					})
				})

				r.Route("/notification-channels", func(r chi.Router) {
					// OKUMA: üyelik yeterli — "kesinti olunca kime haber
					// gidiyor" salt-okunur panonun parçası.
					r.Get("/", notificationEndpoints.list)

					// YAZMA: admin+ (0.1 §2'nin yetki tablosu). Bir kanal
					// eklemek, organizasyonun kesinti bilgisini dışarıya
					// akıtmak demek; test bildirimi de sunucuyu dışarıya
					// istek attıran bir yan etki (2.2'nin manuel kontrol
					// kararıyla aynı gerekçe).
					r.Group(func(r chi.Router) {
						r.Use(requireOrgRole(store.OrgRoleAdmin, deps.Logger))

						r.Post("/", notificationEndpoints.create)
						r.Delete("/{channelID}", notificationEndpoints.remove)
						r.Post("/{channelID}/test", notificationEndpoints.test)
					})
				})

				r.Route("/status-page", func(r chi.Router) {
					// OKUMA: üyelik yeterli — hangi servislerin yayımlandığı
					// salt-okunur panonun parçası.
					r.Get("/", statusPageEndpoints.get)

					// YAZMA: admin+ (0.1 §2'de "status page özelleştirme"
					// açıkça admin yetkisi). Bu iki uç dışarıya NE GÖRÜNDÜĞÜNÜ
					// belirliyor; yanlış kullanımı private bir servisi ifşa
					// etmek demek.
					r.Group(func(r chi.Router) {
						r.Use(requireOrgRole(store.OrgRoleAdmin, deps.Logger))

						r.Patch("/", statusPageEndpoints.update)
						// `/services` (5.3): seçim ve gruplama tek yazmada
						// değiştiği için uç de tek — 5.1'in `/items` yolu bu
						// gövdeyi taşıyamazdı (adı da yalnızca servisleri
						// anlatıyordu).
						r.Put("/services", statusPageEndpoints.replaceServices)
					})
				})
			})
		})
	})

	return r
}
