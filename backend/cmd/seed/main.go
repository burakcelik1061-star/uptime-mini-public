// Command seed, çalıştırılabilir bir demo ortamı üretir (7.4).
//
//	go run ./cmd/seed            # demo yoksa üretir
//	go run ./cmd/seed -reset     # varsa siler ve yeniden üretir
//
// Hiçbir dış hesap gerekmiyor; tek ihtiyaç migration'ları uygulanmış bir
// veritabanı (`docker compose up -d`).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/config"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/logging"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/seed"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed başarısız: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	reset := flag.Bool("reset", false, "var olan demo verisini sil ve yeniden üret")
	flag.Parse()

	config.LoadDotEnv()

	cfg, err := config.LoadMigrate()
	if err != nil {
		return err
	}

	logger := logging.New(string(cfg.Env), "seed")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		return fmt.Errorf("veritabanına bağlanılamadı: %w", err)
	}
	defer pool.Close()

	summary, err := seed.Run(ctx, pool, seed.Options{Reset: *reset})
	if err != nil {
		return err
	}

	logger.Info("demo verisi üretildi",
		slog.String("org", summary.OrgID.String()),
		slog.Int("monitors", summary.Monitors),
		slog.Int("daily_rollups", summary.DailyRollups),
		slog.Int("raw_checks", summary.RawChecks),
		slog.Int("incidents", summary.Incidents))

	// Giriş bilgileri STDOUT'a düz metin: logu okuyan değil, terminalde
	// bekleyen insan için.
	fmt.Printf(`
Demo hazır.

  Pano        : http://localhost:3100
  Status page : http://localhost:3100/status/%s
                http://%s.localhost:3100

  VİTRİN HESABI (Pro planda — 90 günlük geçmiş, kesintiler, temalı sayfa)
    E-posta   : %s
    Parola    : %s

  KOTASINA DAYANMIŞ HESAP (Ücretsiz planda — 5/5 monitor dolu)
    E-posta   : %s
    Parola    : %s
    Denemek için: 6. monitor eklemeyi ya da aralığı 300 sn'nin altına
    çekmeyi deneyin; Plan ekranından Pro'ya geçince ikisi de açılıyor.
    (Ödeme MOCK: hiçbir tahsilat yapılmıyor.)

  POSTA KUTUSU: http://localhost:3100/mailbox
    E-posta bu projede MOCK. Gönderilenler buraya düşüyor ve linkler
    ÇALIŞIYOR: bekleyen davet, abonelik onayı ve abonelikten çıkma.
    Yalnızca APP_ENV açıkça development|test iken açık.
`, summary.StatusPageSlug, summary.StatusPageSlug,
		seed.DemoEmail, seed.DemoPassword,
		seed.FreeDemoEmail, seed.FreeDemoPassword)

	return nil
}
