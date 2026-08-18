// Command worker, arka plan sürecidir: HTTP dinlemez.
//
// api'DEN BAĞIMSIZ bir süreç (bkz. docs/ adım dokümanları): api kapalıyken
// de çalışır, ayrı ayrı yeniden başlatılabilir/ölçeklenebilirler ve worker'ın
// yaptığı iş (dış dünyaya HTTP kontrolleri) api'nin istek gecikmesini
// etkilemez. Ortak kod `internal/` üzerinden paylaşılıyor — aynı Go modülü,
// ayrı binary.
//
// İki mod:
//
//	worker              → süreci çalıştırır (job'lar + kalp atışı)
//	worker healthcheck  → kalp atışı dosyasının tazeliğine bakar (compose)
//
// Kayıtlı job'lar: veritabanı erişilebilirlik yoklaması (3.1), zamanlanmış
// kontrol scheduler'ı (3.2 — durum makinesi 3.3 ve kesinti defteri 4.1 onun
// içinde), özet üretimi ve budama (3.4).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/config"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/logging"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/rollups"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/scheduler"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/subscribers"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/worker"
)

// commandHealthcheck, canlılık kontrolü alt komutu.
//
// Neden ayrı bir binary/script değil: konteynerde `wget` gibi bir araca
// gerek kalmıyor (api'nin healthcheck'i wget kullanıyor ama orada HTTP var),
// kalp atışı biçimini yazan ve okuyan kod aynı pakette duruyor — biçim
// değişirse ikisi birlikte değişiyor.
const commandHealthcheck = "healthcheck"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Logger henüz kurulamamış olabilir; son çare stderr.
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Geliştirme kolaylığı: repo kökündeki .env yüklenir (varsa).
	// Gerçek ortam değişkenleri her zaman kazanır — bkz. config.LoadDotEnv.
	config.LoadDotEnv()

	switch {
	case len(args) == 0:
		return runWorker()
	case args[0] == commandHealthcheck && len(args) == 1:
		return runHealthcheck()
	default:
		return fmt.Errorf("bilinmeyen komut: %q (kullanım: worker [%s])",
			strings.Join(args, " "), commandHealthcheck)
	}
}

// runWorker, süreci ayağa kaldırır ve kapanma sinyaline kadar çalıştırır.
func runWorker() error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}

	logger := logging.New(string(cfg.Env), "worker")

	// NotifyContext: Ctrl+C (SIGINT) veya container durdurma (SIGTERM)
	// geldiğinde context iptal olur; runtime'ın drain mantığı buna bağlı.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// worker api'den AYRI bir havuz açıyor (bkz. config.DatabaseConfig):
	// iki süreç aynı Postgres'e bağlanıyor ama bağlantı bütçeleri ayrı.
	pool, err := postgres.NewPool(ctx, cfg.Database.URL, postgres.PoolOptions{
		MaxConns: cfg.Database.MaxConns,
	})
	if err != nil {
		// Bağlantı kurulamaması ÖLÜMCÜL DEĞİL (0.3'ün kararı): süreç ayakta
		// kalır, havuz kendini toparlayınca db-probe job'ı "yeniden
		// erişilebilir" satırını düşürür.
		logger.Warn("veritabanına açılışta ulaşılamadı, yoklamaya devam edilecek",
			slog.String("error", err.Error()))
	}
	if pool == nil {
		return fmt.Errorf("bağlantı havuzu kurulamadı: %w", err)
	}
	defer pool.Close()

	runtime := worker.New(worker.Options{
		Logger:        logger,
		ShutdownGrace: cfg.ShutdownTimeout,
		Heartbeat: worker.Heartbeat{
			Path:   cfg.Heartbeat.Path,
			Period: cfg.Heartbeat.Period,
		},
	})

	if err := runtime.Register(worker.NewDBProbe(pool, logger).Job(cfg.DBProbePeriod)); err != nil {
		return err
	}

	// ORTAK KOD `internal/` ÜZERİNDEN PAYLAŞILIYOR (3.1'in gerekçesi): sorgu
	// katmanı api ile aynı (`store`), kontrolü çalıştıran paket manuel
	// "şimdi kontrol et" ucuyla aynı (`checker`), sonucu saklayan paket 2.3'ün
	// yazdığı paket (`results`). Zamanlama bunları birbirine bağlıyor.
	queries := store.New(pool)
	schedule := scheduler.NewService(
		queries,
		// Tek bir Checker paylaşılıyor: aktarım katmanı (bağlantı ayarları,
		// zaman aşımları) her kontrolde yeniden kurulmasın.
		checker.New(cfg.DialPolicy),
		results.NewService(queries, logger),
		// Kesinti defteri (4.1): durum değişimi incident açıp kapatıyor.
		// api'de KAYITLI DEĞİL — otomatik incident'ın tek kaynağı zamanlanmış
		// kontroller, yani bu süreç (manuel kontrol durumu zaten değiştirmiyor).
		incidents.NewService(pool, queries, logger),
		// CANLI YAYIN (6.2): olay BURADA doğuyor ama WebSocket api'de yaşıyor —
		// ikisi ayrı süreç (3.1). Taşıma yolu Postgres `LISTEN/NOTIFY` (6.1).
		// Public kanalın kapısı 5.1'in KENDİ sorgularına soruluyor: yayımlanmamış
		// bir servisin durum değişikliği public kanala hiç çıkmıyor.
		realtime.NewBroadcaster(
			realtime.NewNotifier(queries, logger),
			statuspage.NewService(pool, queries, logger),
			logger,
		),
		logger,
		scheduler.Options{MaxConcurrentChecks: cfg.MaxConcurrentChecks},
	)

	if err := runtime.Register(schedule.Job(cfg.SchedulerPeriod)); err != nil {
		return err
	}

	// BİLDİRİMLER (6.3): kuyruğa yazma da gönderim de BURADA, incident yazma
	// yolunda değil. 4.1'in ApplyTransition'ına bir "bildirim üret" adımı
	// eklenseydi, kesinti kaydı yazmak kanal listesini okumaya bağımlı olurdu.
	// Ayrı bir goroutine DEĞİL, runtime'a takılı bir job (3.1'in kuralı).
	// Kayıt TEK: aşağıdaki abone göndericisi de aynı adapter'ları kullanıyor
	// ve "gerçek sağlayıcıya geçiş tek bir yapıcıda" (6.3) sözü ancak bu
	// yapıcının bir kez çağrılmasıyla görünür kalıyor.
	// GELİŞTİRİCİ POSTA KUTUSU (8.2) worker'da da bağlanıyor: kesinti ve
	// abone bildirimleri BURADAN gidiyor ve ekranda görünmeseydi posta
	// kutusu gerçeğin yalnızca yarısını (api'nin gönderdiklerini) gösterirdi.
	// Kapı api ile aynı: APP_ENV açıkça development|test (config.DevTools).
	var outbox *notifications.Outbox
	if cfg.DevTools {
		outbox = notifications.NewOutbox(queries)
	}

	channels := notifications.NewRegistry(cfg.DialPolicy, outbox, logger)

	// MOCK ADAPTER ÜRETİMDE SESSİZ KALMAMALI (7.5): gönderim defteri `sent`
	// yazmaya devam ettiği için "kimse e-posta almıyor" durumu kendiliğinden
	// görünmüyor.
	for _, warning := range channels.MockWarnings(cfg.Env == config.EnvProduction) {
		logger.Warn(warning)
	}

	notifier := notifications.NewDispatcher(
		queries,
		channels,
		logger,
		notifications.DispatcherOptions{},
	)

	if err := runtime.Register(notifier.Job(cfg.NotificationPeriod)); err != nil {
		return err
	}

	// ABONE BİLDİRİMLERİ (6.4): iskelet 6.3'ün aynısı ama ziyaretçiye giden
	// bir bildirim 5.1'in SIZINTI KURALINA tabi — görünürlük kararı
	// statuspage'in kendi sorgularına soruluyor, burada yeniden yazılmıyor.
	mailer, err := channels.For(store.NotificationChannelKindEmail)
	if err != nil {
		return err
	}

	subscriberNotifier := subscribers.NewDispatcher(
		queries,
		statuspage.NewService(pool, queries, logger),
		mailer,
		cfg.PublicBaseURL,
		logger,
	)

	if err := runtime.Register(subscriberNotifier.Job(cfg.NotificationPeriod)); err != nil {
		return err
	}

	// Toplulaştırma ve budama (3.4): ağır analitik sorgular BİLİNÇLİ olarak
	// worker'da — api'nin istek gecikmesini etkilememeleri 3.1'in "ayrı süreç"
	// gerekçesinin bir parçası.
	rollup := rollups.NewService(queries, logger, rollups.Options{
		RawRetentionDays: cfg.RawRetentionDays,
	})

	if err := runtime.Register(rollup.RollupJob(cfg.RollupPeriod)); err != nil {
		return err
	}

	if err := runtime.Register(rollup.RetentionJob(cfg.RetentionPeriod)); err != nil {
		return err
	}

	logger.Info("worker başladı",
		slog.Int("pid", os.Getpid()),
		slog.String("env", string(cfg.Env)),
		slog.Any("jobs", runtime.JobNames()),
		slog.Int("max_concurrent_checks", cfg.MaxConcurrentChecks),
		slog.String("heartbeat_file", cfg.Heartbeat.Path),
		slog.Duration("shutdown_grace", cfg.ShutdownTimeout))

	if err := runtime.Run(ctx); err != nil {
		return err
	}

	logger.Info("worker kapandı")

	return nil
}

// runHealthcheck, kalp atışı dosyasının tazeliğine bakar.
//
// Çıkış kodu 0 = canlı, 1 = değil. Veritabanına BAKMIYOR: liveness ile
// readiness ayrımı (0.3) burada da geçerli — veritabanı düştüğünde worker'ı
// yeniden başlatmak veritabanını geri getirmez.
func runHealthcheck() error {
	heartbeat, err := config.LoadWorkerHeartbeat()
	if err != nil {
		return err
	}

	return worker.CheckHeartbeat(
		heartbeat.Path,
		time.Now(),
		heartbeat.Period*worker.HeartbeatStaleFactor,
	)
}
