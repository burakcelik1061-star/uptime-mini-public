// Command migrate, veritabanı şemasını goose ile yönetir.
//
// Ayrı bir binary olması bilinçli: api ve worker açılışta migration
// ÇALIŞTIRMAZ. İki süreç aynı anda ayağa kalktığında ikisi de migration
// uygulamaya çalışsaydı yarış oluşurdu; ayrıca "uygulama başlarken şemayı
// değiştirmek" geri alınması zor bir davranış. Docker Compose'da bu binary
// tek seferlik bir servis olarak, api/worker'dan ÖNCE çalışır.
//
// Kullanım: migrate up | down | status | version | create <ad> sql
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/db"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/config"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/logging"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migrateTimeout, tüm migration çalışmasının üst sınırı. Kilitlenmiş bir
// migration'ın CI işini süresiz asmasını engeller.
const migrateTimeout = 2 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	// Geliştirme kolaylığı: repo kökündeki .env yüklenir (varsa).
	// Gerçek ortam değişkenleri her zaman kazanır — bkz. config.LoadDotEnv.
	config.LoadDotEnv()

	cfg, err := config.LoadMigrate()
	if err != nil {
		return err
	}

	logger := logging.New(string(cfg.Env), "migrate")

	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(goose.NopLogger())

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	// goose `database/sql` ile çalışıyor; pgx'in stdlib sürücüsü üzerinden
	// aynı bağlantı dizesini kullanıyoruz (uygulama pgxpool kullanıyor).
	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("veritabanı açılamadı: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// Bağlamlı sürümler: migration uzun sürebilir, ama bağlantı yoklaması
	// ve komutun kendisi iptal edilebilir olmalı (CI'da asılı kalmasın).
	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("veritabanına ulaşılamıyor: %w", err)
	}

	args := os.Args[2:]
	if err := goose.RunContext(ctx, command, sqlDB, db.MigrationsDir, args...); err != nil {
		return fmt.Errorf("%q komutu başarısız: %w", command, err)
	}

	version, err := goose.GetDBVersion(sqlDB)
	if err != nil {
		return err
	}

	logger.Info("migration tamamlandı", "command", command, "version", version)

	return nil
}
