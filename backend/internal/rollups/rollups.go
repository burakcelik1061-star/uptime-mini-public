// Package rollups, zaman serisini ÖZETLER ve ham veriyi BUDAR (Faz 3.4).
//
// Neden var (0.1 §6): 60 saniye aralıkla izlenen 100 monitor yılda ~52 milyon
// ham satır üretiyor. 90 günlük status page grafiği (5.2) monitor başına
// ~130 bin ham satır yerine 90 özet satırı okuyacak — bu bir optimizasyon
// değil, tasarımın kendisi: ham veri 30 gün sonra siliniyor, geçmiş yalnızca
// özetlerde yaşıyor.
//
// İki job, iki sorumluluk:
//
//	rollup    → ham sonuçlardan saatlik ve günlük özetler (idempotent upsert)
//	retention → toplulaştırması yapılmış ham partition'ları düşürür,
//	            saatlik özetleri budar, partition penceresini ileri kaydırır
//
// SIRALAMA HAYATİ: önce rollup, sonra budama. Budama job'ı bunu kendi başına
// garanti ediyor — yalnızca özeti ÜRETİLMİŞ günlere dokunuyor (bkz.
// RunRetention). Aksi hâlde rollup'ın bir gün çalışmaması, geri dönüşü
// olmayan veri kaybı demekti.
package rollups

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/worker"
)

// Job adları (runtime loglarında görünür).
const (
	RollupJobName    = "rollup"
	RetentionJobName = "retention"
)

// Varsayılan saklama süreleri — 0.1 §6.3.
const (
	// DefaultRawRetentionDays: monitor detay sayfasının "son kontroller" ve
	// yanıt süresi grafiği bu aralığı kullanıyor; daha eskisi için özetler
	// yeterli.
	DefaultRawRetentionDays = 30

	// DefaultHourlyRetentionDays: 13 ay — yıl-üstü karşılaştırma yapılabilsin.
	// Günlük özetler HİÇ silinmiyor (yılda ~36 bin satır; budamanın maliyeti
	// faydasından büyük).
	DefaultHourlyRetentionDays = 400

	// DefaultPartitionDaysAhead: partition penceresi hep bugün + bu kadar gün
	// (2.3'ün açılış penceresiyle aynı).
	DefaultPartitionDaysAhead = 30
)

const (
	// hourlyLookback, su seviyesi güncelken bile geriye dönük yeniden
	// hesaplanan pencere.
	//
	// Neden gerekli: bir kontrol, ait olduğu saat kapandıktan SONRA yazılmış
	// olabilir (yavaş bir kontrol 23:59'da başlayıp 00:00'dan sonra biter).
	// Upsert idempotent olduğu için yeniden hesaplamanın bedeli yok.
	hourlyLookback = 3 * time.Hour

	// dailyLookback, aynı gerekçeyle günlük kovalar için.
	dailyLookback = day

	// maxHourlySpan / maxDailySpan, TEK BİR TURDA işlenecek en geniş aralık.
	//
	// Worker uzun süre kapalı kaldıysa su seviyesi çok geride olabilir; tek
	// sorguda haftaların ham verisini taramak, veritabanını dakikalarca meşgul
	// eden bir sorgu demekti. Job her turda bir parça ilerliyor ve birkaç turda
	// yetişiyor.
	maxHourlySpan = 48 * time.Hour
	maxDailySpan  = 30 * day

	// day, "bir gün" — `time` paketinde sabiti yok.
	day = 24 * time.Hour
)

// Su seviyesi kayıtlarının adları (rollup_state tablosu).
const (
	stateHourly = "hourly"
	stateDaily  = "daily"
)

// Store, bu paketin ihtiyaç duyduğu sorgular.
type Store interface {
	UpsertHourlyRollups(ctx context.Context, arg store.UpsertHourlyRollupsParams) (int64, error)
	UpsertDailyRollups(ctx context.Context, arg store.UpsertDailyRollupsParams) (int64, error)
	GetRollupState(ctx context.Context, name string) (time.Time, error)
	SetRollupState(ctx context.Context, arg store.SetRollupStateParams) error
	OldestScheduledCheck(ctx context.Context) (time.Time, error)
	DeleteHourlyRollupsBefore(ctx context.Context, beforeTime time.Time) (int64, error)
	EnsureCheckResultPartitions(ctx context.Context, daysAhead int32) (int32, error)
	DropCheckResultPartitionsBefore(ctx context.Context, beforeTime time.Time) (int32, error)
}

// Options, saklama süreleri (0 verilirse varsayılan).
type Options struct {
	RawRetentionDays    int
	HourlyRetentionDays int
	PartitionDaysAhead  int
}

// Service, toplulaştırma ve budama.
type Service struct {
	queries Store
	logger  *slog.Logger
	opts    Options

	// now, testlerin saati sabitleyebilmesi için.
	now func() time.Time
}

// NewService, servisi kurar.
func NewService(queries Store, logger *slog.Logger, opts Options) *Service {
	if opts.RawRetentionDays <= 0 {
		opts.RawRetentionDays = DefaultRawRetentionDays
	}
	if opts.HourlyRetentionDays <= 0 {
		opts.HourlyRetentionDays = DefaultHourlyRetentionDays
	}
	if opts.PartitionDaysAhead <= 0 {
		opts.PartitionDaysAhead = DefaultPartitionDaysAhead
	}

	return &Service{queries: queries, logger: logger, opts: opts, now: time.Now}
}

// RollupStats, bir toplulaştırma turunun sonucu.
type RollupStats struct {
	HourlyBuckets int64
	DailyBuckets  int64
	From          time.Time
	To            time.Time
}

// RetentionStats, bir budama turunun sonucu.
type RetentionStats struct {
	DroppedPartitions  int32
	DeletedHourlyRows  int64
	CreatedPartitions  int32
	RawCutoff          time.Time
	AggregationBlocked bool
}

// RollupJob, toplulaştırmayı runtime'a kaydedilebilir bir job'a çevirir.
func (s *Service) RollupJob(interval time.Duration) worker.Job {
	return worker.Job{
		Name:     RollupJobName,
		Interval: interval,
		Run: func(ctx context.Context) error {
			_, err := s.RunRollup(ctx)

			return err
		},
	}
}

// RetentionJob, budamayı runtime'a kaydedilebilir bir job'a çevirir.
func (s *Service) RetentionJob(interval time.Duration) worker.Job {
	return worker.Job{
		Name:     RetentionJobName,
		Interval: interval,
		Run: func(ctx context.Context) error {
			_, err := s.RunRetention(ctx)

			return err
		},
	}
}

// RunRollup, saatlik ve günlük özetleri günceller.
//
// Pencere SU SEVİYESİNDEN türetiliyor (son işlenmiş kova): "son 3 saat" gibi
// sabit bir pencere olsaydı, worker bir hafta kapalı kaldığında o haftanın
// kovaları sonsuza kadar boş kalırdı. Geriye dönük doldurma (backfill) ayrı
// bir kod yolu değil — aynı job, birkaç turda yetişiyor.
func (s *Service) RunRollup(ctx context.Context) (RollupStats, error) {
	now := s.now().UTC()

	hourly, err := s.rollupWindow(ctx, stateHourly, now, time.Hour, hourlyLookback, maxHourlySpan)
	if err != nil {
		return RollupStats{}, err
	}

	stats := RollupStats{From: hourly.from, To: hourly.to}

	if hourly.to.After(hourly.from) {
		stats.HourlyBuckets, err = s.queries.UpsertHourlyRollups(ctx, store.UpsertHourlyRollupsParams{
			FromTime: hourly.from,
			ToTime:   hourly.to,
		})
		if err != nil {
			return stats, fmt.Errorf("saatlik özetler üretilemedi: %w", err)
		}

		if err := s.queries.SetRollupState(ctx, store.SetRollupStateParams{
			Name: stateHourly, ProcessedThrough: hourly.to,
		}); err != nil {
			return stats, fmt.Errorf("saatlik su seviyesi yazılamadı: %w", err)
		}
	}

	daily, err := s.rollupWindow(ctx, stateDaily, now, day, dailyLookback, maxDailySpan)
	if err != nil {
		return stats, err
	}

	if daily.to.After(daily.from) {
		stats.DailyBuckets, err = s.queries.UpsertDailyRollups(ctx, store.UpsertDailyRollupsParams{
			FromTime: daily.from,
			ToTime:   daily.to,
		})
		if err != nil {
			return stats, fmt.Errorf("günlük özetler üretilemedi: %w", err)
		}

		if err := s.queries.SetRollupState(ctx, store.SetRollupStateParams{
			Name: stateDaily, ProcessedThrough: daily.to,
		}); err != nil {
			return stats, fmt.Errorf("günlük su seviyesi yazılamadı: %w", err)
		}
	}

	if stats.HourlyBuckets > 0 || stats.DailyBuckets > 0 {
		s.logger.Info("özetler güncellendi",
			slog.Int64("hourly_buckets", stats.HourlyBuckets),
			slog.Int64("daily_buckets", stats.DailyBuckets),
			slog.Time("from", hourly.from),
			slog.Time("to", hourly.to))
	}

	return stats, nil
}

// window, tek bir turda işlenecek aralık [from, to).
type window struct {
	from time.Time
	to   time.Time
}

// rollupWindow, su seviyesinden ve saatten pencereyi hesaplar.
//
// Üç kural bir arada:
//  1. Su seviyesinden DEVAM: worker kapalı kaldıysa kaldığı yerden sürüyor.
//  2. Son `lookback` kadarı HER TURDA yeniden hesaplanıyor: ait olduğu kova
//     kapandıktan sonra yazılan kontroller (yavaş bir kontrol 23:59'da başlayıp
//     00:00'dan sonra biter) özete girsin. Upsert idempotent, bedeli yok.
//  3. Tek turda en fazla `maxSpan`: su seviyesi haftalarca geride kaldıysa tek
//     bir sorgu veritabanını dakikalarca meşgul etmesin. Job her turda bir
//     parça ilerliyor.
func (s *Service) rollupWindow(
	ctx context.Context, name string, now time.Time, bucket, lookback, maxSpan time.Duration,
) (window, error) {
	// Üst sınır DEVAM EDEN kovayı da kapsıyor: kova dolmadan da grafik veri
	// gösterebilsin. Yarım kova bir anlık görüntü — kova kapandığında aynı
	// upsert onu düzeltiyor (idempotentliğin ikinci faydası).
	end := now.Truncate(bucket).Add(bucket)

	start, err := s.windowStart(ctx, name, now, bucket, lookback)
	if err != nil {
		return window{}, err
	}

	if end.Sub(start) > maxSpan {
		end = start.Add(maxSpan)
	}

	return window{from: start, to: end}, nil
}

// windowStart, toplulaştırmanın nereden devam edeceğini söyler.
func (s *Service) windowStart(
	ctx context.Context, name string, now time.Time, bucket, lookback time.Duration,
) (time.Time, error) {
	processed, err := s.queries.GetRollupState(ctx, name)
	if err == nil {
		start := processed.UTC().Truncate(bucket)

		// Son `lookback` kadarı her turda yeniden hesaplanıyor (geç yazılan
		// satırlar); su seviyesi zaten daha gerideyse ona dokunulmuyor.
		if recent := now.Truncate(bucket).Add(-lookback); recent.Before(start) {
			start = recent
		}

		return start, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%s su seviyesi okunamadı: %w", name, err)
	}

	// İLK ÇALIŞMA: su seviyesi yok. Ham verinin en başından başlıyoruz — mevcut
	// bir veritabanına bu sürüm ilk kez kurulduğunda geçmiş de özetlensin.
	oldest, err := s.queries.OldestScheduledCheck(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Hiç ham veri yok: yapılacak iş de yok.
			return now.Truncate(bucket), nil
		}

		return time.Time{}, fmt.Errorf("en eski kontrol okunamadı: %w", err)
	}

	return oldest.UTC().Truncate(bucket), nil
}

// RunRetention, ham veriyi ve eski saatlik özetleri budar.
//
// İKİ KAPI: bir günün ham partition'ı ancak (1) saklama süresinden eskiyse ve
// (2) GÜNLÜK ÖZETİ ÜRETİLMİŞSE düşürülüyor. İkincisi olmadan, toplulaştırma
// job'ının bozulduğu bir hafta sessizce geri dönülemez veri kaybına dönerdi
// (0.1 §6.3: "önce rollup, sonra prune").
func (s *Service) RunRetention(ctx context.Context) (RetentionStats, error) {
	now := s.now().UTC()
	stats := RetentionStats{}

	// Partition penceresini ileri kaydır. ÖNCE bu yapılıyor: budama hata verse
	// bile yeni kontroller yazılabilir kalmalı (DEFAULT partition yok, 2.3).
	created, err := s.queries.EnsureCheckResultPartitions(ctx, int32(s.opts.PartitionDaysAhead)) //nolint:gosec // yapılandırmadan doğrulanmış
	if err != nil {
		return stats, fmt.Errorf("partition penceresi ilerletilemedi: %w", err)
	}
	stats.CreatedPartitions = created

	ageCutoff := now.AddDate(0, 0, -s.opts.RawRetentionDays).Truncate(day)

	// İKİNCİ KAPI: toplulaştırma nereye kadar geldi?
	//
	// Soru bilinçli olarak "hangi günün özeti var?" DEĞİL: hiç kontrol
	// üretmemiş bir gün özet satırı da üretmiyor, yani veriye bakarak "bu gün
	// işlendi mi" sorusu cevaplanamaz. Su seviyesi tam olarak bunu söylüyor
	// (bkz. rollup_state).
	processed, err := s.queries.GetRollupState(ctx, stateDaily)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return stats, fmt.Errorf("günlük su seviyesi okunamadı: %w", err)
		}

		// Toplulaştırma hiç çalışmamış → HAM VERİ BUDANMIYOR.
		//
		// Ama saatlik özetlerin budaması YİNE DE yapılıyor: onun kapısı
		// toplulaştırma değil YAŞ (13 ay). İkisini aynı `return`e bağlamak,
		// ham veri budaması bloke olduğunda saatlik tablonun sonsuza kadar
		// büyümesi demekti.
		stats.AggregationBlocked = true
		s.logger.Warn("ham veri budanmadı: toplulaştırma henüz çalışmadı")

		if err := s.pruneHourlyRollups(ctx, now, &stats); err != nil {
			return stats, err
		}

		return stats, nil
	}

	// Su seviyesi HARİÇ bir sınır: o ana kadar her şey işlendi.
	aggregatedCutoff := processed.UTC().Truncate(day)

	cutoff := ageCutoff
	if aggregatedCutoff.Before(cutoff) {
		cutoff = aggregatedCutoff
		stats.AggregationBlocked = true
	}

	stats.RawCutoff = cutoff

	dropped, err := s.queries.DropCheckResultPartitionsBefore(ctx, cutoff)
	if err != nil {
		return stats, fmt.Errorf("ham veri partition'ları düşürülemedi: %w", err)
	}
	stats.DroppedPartitions = dropped

	if err := s.pruneHourlyRollups(ctx, now, &stats); err != nil {
		return stats, err
	}

	if stats.DroppedPartitions > 0 || stats.DeletedHourlyRows > 0 || stats.CreatedPartitions > 0 {
		s.logger.Info("budama tamamlandı",
			slog.Int("dropped_partitions", int(stats.DroppedPartitions)),
			slog.Int64("deleted_hourly_rows", stats.DeletedHourlyRows),
			slog.Int("created_partitions", int(stats.CreatedPartitions)),
			slog.Time("raw_cutoff", cutoff),
			slog.Bool("aggregation_blocked", stats.AggregationBlocked))
	}

	return stats, nil
}

// pruneHourlyRollups, saklama süresinden (13 ay) eski saatlik özetleri siler.
//
// Ham veri budamasından AYRI: bu tablonun kapısı toplulaştırma değil YAŞ.
// Günlük özetler hiç silinmiyor (0.1 §6.3).
func (s *Service) pruneHourlyRollups(ctx context.Context, now time.Time, stats *RetentionStats) error {
	deleted, err := s.queries.DeleteHourlyRollupsBefore(ctx,
		now.AddDate(0, 0, -s.opts.HourlyRetentionDays).Truncate(day))
	if err != nil {
		return fmt.Errorf("eski saatlik özetler silinemedi: %w", err)
	}

	stats.DeletedHourlyRows = deleted

	return nil
}
