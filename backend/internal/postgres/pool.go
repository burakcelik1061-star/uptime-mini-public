// Package postgres, pgx bağlantı havuzunu kurar.
//
// Neden pgx (database/sql değil): Postgres'e özgü tipleri (enum, array,
// jsonb, timestamptz) doğrudan destekliyor, hazırlanmış ifadeleri kendi
// yönetiyor ve sqlc'nin ürettiği kod da pgx arayüzünü hedefliyor.
// `database/sql` yalnızca migration'da (goose gerektirdiği için) kullanılıyor.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOptions, havuzun davranışını belirler.
type PoolOptions struct {
	MaxConns int32
	// ConnectTimeout, açılıştaki ilk yoklamanın süre sınırı.
	ConnectTimeout time.Duration
}

// NewPool bir bağlantı havuzu oluşturur ve bağlantıyı bir kez yoklar.
//
// Yoklama BAŞARISIZ olursa hata döner ama havuz yine de kullanılabilir
// durumdadır: çağıran taraf (api/worker) bunu ölümcül saymayıp uyarı
// loglayabilir. Gerekçe: veritabanı henüz ayağa kalkmamışken uygulamanın
// çökmesi yerine, hazır olmadığını `/ready` üzerinden bildirmesi tercih
// ediliyor — orkestratör sıralamayı kendi çözer.
func NewPool(ctx context.Context, url string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL çözümlenemedi: %w", err)
	}

	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}

	// Uzun ömürlü bağlantılar sunucu tarafındaki ayar değişikliklerini ve
	// bağlantı sızıntılarını gizler; sınırlı ömür bunu önler.
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("bağlantı havuzu kurulamadı: %w", err)
	}

	timeout := opts.ConnectTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		return pool, fmt.Errorf("veritabanına ulaşılamadı: %w", err)
	}

	return pool, nil
}
