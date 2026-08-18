// Command api, HTTP API sürecidir.
//
// worker'dan tamamen bağımsız çalışır: worker kapalıyken de istekleri
// karşılar, worker'ın yeniden başlatılması api'yi etkilemez
// (bkz. docs/0.2-tech-stack.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/billing"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/config"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/httpapi"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/logging"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/maintenance"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/passwordreset"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/sessions"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/subscribers"
)

func main() {
	if err := run(); err != nil {
		// Logger henüz kurulamamış olabilir; son çare stderr.
		fmt.Fprintf(os.Stderr, "api başlatılamadı: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Geliştirme kolaylığı: repo kökündeki .env yüklenir (varsa).
	// Gerçek ortam değişkenleri her zaman kazanır — bkz. config.LoadDotEnv.
	config.LoadDotEnv()

	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}

	// GÜVENİLEN KAYNAK LİSTESİ BOOT'TA DOĞRULANIYOR: bozuk bir CIDR sessizce
	// "hiçbir kaynağa güvenme"ye düşseydi, sınır beklenenden farklı bir
	// kimlikle çalışır ve bunu kimse fark etmezdi. Ayrıştırıcı kuralı UYGULAYAN
	// pakette (httpapi) duruyor; ikinci bir kopya, iki kuralın ayrışması demek.
	if err := httpapi.ParseTrustedProxies(cfg.TrustedProxies); err != nil {
		return fmt.Errorf("TRUSTED_PROXIES geçersiz: %w", err)
	}

	logger := logging.New(string(cfg.Env), "api")
	startedAt := time.Now()

	// Yapılandırma uyarıları (ör. geçici imzalama anahtarı üretildi) config
	// yüklenirken toplanıyor; logger ancak burada var.
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}

	// NotifyContext: Ctrl+C (SIGINT) veya container durdurma (SIGTERM)
	// geldiğinde context iptal olur; aşağıdaki graceful shutdown buna bağlı.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.Database.URL, postgres.PoolOptions{
		MaxConns: cfg.Database.MaxConns,
	})
	if err != nil {
		// Bağlantı kurulamaması ÖLÜMCÜL DEĞİL: süreç ayakta kalır ve
		// /ready üzerinden hazır olmadığını bildirir. Veritabanı geri
		// geldiğinde havuz kendiliğinden toparlanır.
		logger.Warn("veritabanına açılışta ulaşılamadı, /ready hazır değil bildirecek",
			slog.String("error", err.Error()))
	}
	if pool == nil {
		return fmt.Errorf("bağlantı havuzu kurulamadı: %w", err)
	}
	defer pool.Close()

	// sqlc'nin ürettiği sorgu katmanı doğrudan pgx havuzunun üzerinde
	// çalışıyor — araya bir ORM girmiyor. Tek örnek: oturum servisi de aynı
	// katmanı (transaction'da `WithTx` ile) kullanıyor.
	queries := store.New(pool)

	// CANLI YAYIN (6.1): hub api sürecinin belleğinde, olaylar ise worker'dan
	// Postgres LISTEN/NOTIFY ile geliyor. Dinleyici sunucudan ÖNCE başlıyor:
	// ilk bağlantı kurulmadan kanalın dinleniyor olması gerekiyor.
	hub := realtime.NewHub(logger)
	go realtime.NewListener(cfg.Database.URL, hub, logger).Run(ctx)

	statusPages := statuspage.NewService(pool, queries, logger)

	// GELİŞTİRİCİ POSTA KUTUSU (8.2): yalnızca APP_ENV açıkça
	// development|test iken kuruluyor (config.devToolsEnabled). nil kalırsa
	// mock adapter 6.3'teki gibi yalnızca loga yazıyor ve ucu router'a hiç
	// girmiyor — kapı tek bir yerde, `cfg.DevTools`ta.
	var outbox *notifications.Outbox
	if cfg.DevTools {
		outbox = notifications.NewOutbox(queries)
	}

	// E-posta gönderimi TEK bir kayıttan (6.3): hem bildirim kanalları hem
	// abonelik e-postaları aynı adapter'ı kullanıyor.
	mailRegistry := notifications.NewRegistry(cfg.DialPolicy, outbox, logger)

	// MOCK ADAPTER ÜRETİMDE SESSİZ KALMAMALI (7.5): gönderim defteri `sent`
	// yazmaya devam ettiği için "kimse e-posta almıyor" durumu kendiliğinden
	// görünmüyor. Gerçek sağlayıcıya geçiş tek bir yapıcıda (NewRegistry).
	for _, warning := range mailRegistry.MockWarnings(cfg.Env == config.EnvProduction) {
		logger.Warn(warning)
	}

	mailer, err := mailRegistry.For(store.NotificationChannelKindEmail)
	if err != nil {
		return err
	}

	// api yalnızca MANUEL hareketleri duyuruyor (6.2). Otomatik durum
	// değişimlerinin duyurusu worker'da, `checkOne`ın içinde.
	broadcaster := realtime.NewBroadcaster(realtime.NewNotifier(queries, logger), statusPages, logger)

	// ÖDEME SAĞLAYICISI (8.5) — MOCK, tek yapıcıdan seçiliyor. Üretimde
	// sessiz kalmamalı: tahsilat yapılmıyor ama planlar yükseliyor.
	payments := billing.NewProvider(logger)
	if warning := billing.MockWarning(payments, cfg.Env == config.EnvProduction); warning != "" {
		logger.Warn(warning)
	}

	// TEK ORGS SERVİSİ, İKİ ROL (8.3): üyelik doğrulaması (1.4) ve davet
	// akışı aynı örnekten geçiyor. İki ayrı örnek kurmak, aynı veritabanı
	// katmanının iki kopyası ve "hangisi hangi bağlantıyı kullanıyor"
	// sorusu demekti.
	orgService := orgs.NewService(pool, queries, mailer, payments, logger)

	server := &http.Server{
		Addr: net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler: httpapi.NewRouter(httpapi.Deps{
			Logger:    logger,
			StartedAt: startedAt,
			DB:        pool,
			Users:     queries,
			Hasher:    auth.NewHasher(),
			Tokens:    auth.NewTokenService(cfg.JWT.AccessSecret, cfg.JWT.AccessTTL),
			Sessions: sessions.NewService(pool, queries,
				auth.NewRefreshTokenFactory(cfg.Refresh.Secret, cfg.Refresh.TTL), logger),
			Orgs: orgService,
			// Davetler (8.3) aynı servisten: davet yönetimi org-scoped
			// ağacın içinde, kabul ucu ise DIŞINDA (bkz. httpapi/router.go).
			Invitations: orgService,
			// Plan & kota (8.5): aynı servis örneği.
			Plans: orgService,
			// Parola sıfırlama (8.4): hash'leyici kayıt/giriş ile AYNI
			// (parolanın kuralları ve maliyeti tek yerden geliyor).
			PasswordReset: passwordreset.NewService(
				pool, queries, auth.NewHasher(), mailer, cfg.PublicBaseURL, logger),
			// Davet linkinin kökü ARAYÜZÜN adresi (6.4'ün kararı).
			PublicBaseURL: cfg.PublicBaseURL,
			Monitors:      monitors.NewService(pool, queries, logger),
			// Tek bir Checker uygulama boyunca paylaşılıyor: aktarım katmanı
			// (bağlantı ayarları, zaman aşımları) her istekte yeniden
			// kurulmasın.
			// SSRF KORUMASI (7.4): manuel kontrol ucu sunucuyu kullanıcının
			// yazdığı adrese istek atmaya zorluyor — 2.2'de "SSRF 7.4'ün
			// konusu" diye bırakılan yer burası.
			Checks:  checker.New(cfg.DialPolicy),
			Results: results.NewService(queries, logger),
			// Kesinti kayıtları: api yalnızca OKUYOR ve timeline'a yazıyor
			// (4.2). Otomatik açma/kapama worker'ın işi — aynı servis, iki
			// süreçte iki farklı yüzü.
			Incidents: incidents.NewService(pool, queries, logger),
			// Bakım pencereleri (4.3): pencere + kapsadığı servisler tek
			// transaction'da yazıldığı için havuzu da alıyor.
			Maintenance: maintenance.NewService(pool, queries, logger),
			// Status page (5.1): hem org-scoped admin uçları hem OTURUMSUZ
			// public uç aynı servisten besleniyor — "yayımlanan servis"in tek
			// bir tanımı olsun. Servis seçimi tek transaction'da değiştiği
			// için havuzu da alıyor.
			StatusPages: statusPages,
			// Bildirim kanalları (6.3): api yalnızca YAPILANDIRMAYI yönetiyor
			// ve test bildirimini senkron gönderiyor. Gerçek bildirimleri
			// worker'daki job gönderiyor — gönderim kesinti yazmayı
			// bekletmemeli.
			Notifications: notifications.NewService(queries, mailRegistry, logger),
			// Abonelikler (6.4): OTURUMSUZ yüzey. E-posta gönderimi 6.3'ün
			// AYNI soyutlamasından geçiyor — ikinci bir e-posta katmanı,
			// gerçek sağlayıcıya geçişi iki yerde yapmak demekti.
			Subscribers: subscribers.NewService(queries, mailer, cfg.PublicBaseURL, logger),
			// Geliştirici posta kutusu (8.2). NIL geçmek "uç kaydedilmesin"
			// demek — router bu alanı tam olarak öyle okuyor.
			Outbox: outbox,
			// WebSocket (6.1). Bilet servisi access token'ın SIRRINI paylaşıyor
			// ama audience'ı ayrı: biri diğerinin yerine kullanılamıyor.
			Realtime: hub,
			Tickets:  auth.NewTicketService(cfg.JWT.AccessSecret, auth.DefaultTicketTTL),
			// api'nin kendi ürettiği olaylar da hub'a DOĞRUDAN değil taşıma
			// yolundan giriyor: kural tek — "olay hub'a yalnızca dinleyiciden
			// girer". Çok kopyalı bir api'de diğer kopyalara bağlı istemciler
			// de olayı böylece görüyor.
			Notifier:    realtime.NewNotifier(queries, logger),
			Broadcaster: broadcaster,
			// CORS: tarayıcı arayüzü (:3100) api ile farklı origin'de.
			//
			// PUBLIC STATUS SAYFASI BU LİSTEYE HİÇ GİRMİYOR: sayfa Next
			// sunucusunda render ediliyor (SSR), yani api'yi tarayıcı değil
			// sunucu çağırıyor ve tarayıcı origin'i devreye girmiyor
			// (bkz. docs/5.1-public-status-page.md).
			WebOrigins: cfg.WebOrigins,
			// Secure cookie yalnızca üretimde: local geliştirme http
			// üzerinden koşuyor ve Secure cookie orada hiç gönderilmezdi.
			SecureCookies: cfg.Env == config.EnvProduction,
			// Hız sınırının istemci kimliği (7.5): liste boşken ham TCP
			// adresi, doluyken o kaynakların ilettiği ziyaretçi adresi.
			TrustedProxies: cfg.TrustedProxies,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// KAPANMADA WEBSOCKET'LER DÜZGÜN KAPANSIN: `Shutdown` hijack edilmiş
	// bağlantıları beklemiyor ve iptal de etmiyor — süreç öldüğünde istemci
	// yarım kalmış bir TCP bağlantısıyla kalırdı. Hub'ı kapatmak her bağlantıya
	// "going away" kapanış çerçevesini gönderiyor, yani istemci sebebi bilerek
	// yeniden bağlanıyor.
	server.RegisterOnShutdown(hub.Shutdown)

	// Sunucuyu ayrı bir goroutine'de çalıştırıp ana akışta sinyali bekliyoruz.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api dinlemeye başladı",
			slog.Int("port", cfg.Port),
			slog.String("env", string(cfg.Env)))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("kapanma sinyali alındı, açık istekler bekleniyor",
			slog.Duration("timeout", cfg.ShutdownTimeout))
	}

	// Graceful shutdown: devam eden istekler tamamlansın, yeni bağlantı
	// kabul edilmesin. Süre dolarsa zorla kapatılır.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("düzgün kapanamadı: %w", err)
	}

	logger.Info("api kapandı")

	return nil
}
