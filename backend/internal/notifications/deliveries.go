package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/worker"
)

// Gönderim job'ı (Faz 6.3).
//
// BİLDİRİM GÖNDERİMİ KESİNTİ YAZMAYI BLOKE ETMİYOR ve bu, tasarımın
// tamamını belirleyen kısıt. İki ayrı sonucu var:
//
//  1. KUYRUĞA YAZMA DA INCIDENT YAZMA YOLUNDA DEĞİL. 4.1'in
//     `ApplyTransition`ına "bildirim satırlarını da üret" adımı eklenseydi,
//     kesinti kaydı yazmak kanal listesini okumaya ve fazladan yazmalara
//     bağımlı olurdu. Onun yerine bu job TARIYOR: hangi (kanal × incident ×
//     olay) üçlüsü henüz deftere girmemişse yazıyor. Incident yazma yolu
//     6.3'ten tamamen habersiz.
//  2. GÖNDERİM AYRI BİR SÜREÇTE (worker) ve ayrı bir job olarak — kendi
//     goroutine'i olarak DEĞİL (3.1'in kuralı): runtime'ın panik kurtarma,
//     aralık aşımı uyarısı ve drain'li kapanma davranışları bu işe de
//     uygulansın.
//
// İKİ KEZ GÖNDERMEME garantisi UYGULAMADA DEĞİL ŞEMADA: `(channel_id,
// incident_id, event)` UNIQUE kısıtı + `ON CONFLICT DO NOTHING` (4.1'in kısmi
// unique indeksiyle aynı disiplin). Sahiplenme de 3.2'nin `FOR UPDATE SKIP
// LOCKED` deseniyle — iki worker kopyası aynı satırı ele geçiremiyor.

// JobName, runtime'da görünen job adı.
const JobName = "notifications"

const (
	// DefaultLookback, kuyruğa alınacak olayların yaş sınırı.
	//
	// Worker bir hafta kapalı kaldıysa açılışta bir hafta önceki kesintileri
	// bildirmek, kimsenin okumak istemediği bir bildirim çığı demek. Sınırın
	// diğer yarısı sorguda: bir olay ancak KANAL OLUŞTURULDUKTAN sonra
	// gerçekleştiyse kuyruğa giriyor.
	DefaultLookback = 24 * time.Hour

	// DefaultBatchSize, bir turda gönderilecek en fazla bildirim.
	DefaultBatchSize = 20

	// DefaultMaxAttempts, bir gönderim kaç kez denenir.
	//
	// Sınırsız deneme yok: ulaşılamayan bir hedef (silinmiş webhook, kapanmış
	// posta kutusu) sonsuza kadar tur başına bir hata satırı üretirdi.
	DefaultMaxAttempts = 3

	// DefaultBackoff, denemeler arası taban bekleme (deneme sayısıyla çarpılıyor).
	DefaultBackoff = time.Minute

	// claimLease, sahiplenilen bir satırın başka bir tura düşmeden önce
	// tanınan süresi. Gönderim bu sürede bitmezse (süreç öldü) satır yeniden
	// sıraya giriyor — 3.2'nin claim'iyle aynı fikir.
	claimLease = 5 * time.Minute

	// sendTimeout, tek bir gönderime tanınan süre.
	sendTimeout = 10 * time.Second
)

// DeliveryStore, gönderim job'ının ihtiyaç duyduğu sorgular.
type DeliveryStore interface {
	EnqueueIncidentNotifications(ctx context.Context, lookback pgtype.Interval) (int64, error)
	ClaimPendingDeliveries(
		ctx context.Context, arg store.ClaimPendingDeliveriesParams,
	) ([]store.ClaimPendingDeliveriesRow, error)
	MarkDeliverySent(ctx context.Context, id uuid.UUID) error
	MarkDeliveryFailed(ctx context.Context, arg store.MarkDeliveryFailedParams) error
}

// DispatcherOptions, gönderim ayarları.
type DispatcherOptions struct {
	Lookback    time.Duration
	BatchSize   int
	MaxAttempts int
	Backoff     time.Duration
}

// Dispatcher, bekleyen bildirimleri gönderir.
type Dispatcher struct {
	queries  DeliveryStore
	registry *Registry
	logger   *slog.Logger
	opts     DispatcherOptions
}

// NewDispatcher, göndericiyi kurar.
func NewDispatcher(
	queries DeliveryStore, registry *Registry, logger *slog.Logger, opts DispatcherOptions,
) *Dispatcher {
	if opts.Lookback <= 0 {
		opts.Lookback = DefaultLookback
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.Backoff <= 0 {
		opts.Backoff = DefaultBackoff
	}

	if logger == nil {
		logger = slog.Default()
	}

	return &Dispatcher{queries: queries, registry: registry, logger: logger, opts: opts}
}

// Job, göndericiyi worker runtime'ına kaydedilebilir bir job'a çevirir.
func (d *Dispatcher) Job(interval time.Duration) worker.Job {
	return worker.Job{
		Name:     JobName,
		Interval: interval,
		Run: func(ctx context.Context) error {
			_, err := d.RunOnce(ctx)

			return err
		},
	}
}

// DispatchStats, bir turun sonucu.
type DispatchStats struct {
	// Enqueued, deftere yeni yazılan gönderim sayısı.
	Enqueued int64
	// Sent / Failed, bu turda gönderilen ve başarısız olan sayılar.
	Sent   int
	Failed int
}

// RunOnce, bir tur çalışır: önce kuyruğa yaz, sonra sırası geleni gönder.
func (d *Dispatcher) RunOnce(ctx context.Context) (DispatchStats, error) {
	var stats DispatchStats

	enqueued, err := d.queries.EnqueueIncidentNotifications(ctx, intervalOf(d.opts.Lookback))
	if err != nil {
		return stats, fmt.Errorf("bildirimler kuyruğa alınamadı: %w", err)
	}

	stats.Enqueued = enqueued

	claimed, err := d.queries.ClaimPendingDeliveries(ctx, store.ClaimPendingDeliveriesParams{
		//nolint:gosec // BatchSize yapılandırmadan doğrulanarak geliyor
		RowLimit:   int32(d.opts.BatchSize),
		ClaimLease: intervalOf(claimLease),
	})
	if err != nil {
		return stats, fmt.Errorf("bekleyen bildirimler sahiplenilemedi: %w", err)
	}

	for _, delivery := range claimed {
		if d.deliver(ctx, delivery) {
			stats.Sent++
		} else {
			stats.Failed++
		}
	}

	if stats.Enqueued > 0 || stats.Sent > 0 || stats.Failed > 0 {
		d.logger.Info("bildirim turu tamamlandı",
			slog.Int64("enqueued", stats.Enqueued),
			slog.Int("sent", stats.Sent),
			slog.Int("failed", stats.Failed))
	}

	return stats, nil
}

// deliver, tek bir gönderimi yapar ve sonucu deftere yazar.
func (d *Dispatcher) deliver(ctx context.Context, row store.ClaimPendingDeliveriesRow) bool {
	adapter, err := d.registry.For(row.ChannelKind)
	if err != nil {
		d.markFailed(ctx, row, err)

		return false
	}

	// GÖNDERİMİN KENDİ ZAMAN SINIRI: dış dünyayla konuşan bir iş (bugün mock,
	// yarın gerçek bir sağlayıcı) ve yanıt vermeyen bir hedef tüm turu
	// kilitlememeli.
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	if err := adapter.Send(sendCtx, row.ChannelTarget, messageOf(row)); err != nil {
		d.markFailed(ctx, row, err)

		return false
	}

	if err := d.queries.MarkDeliverySent(ctx, row.ID); err != nil {
		// Gönderim YAPILDI ama defter yazılamadı: satır `pending` kaldığı için
		// bir sonraki turda TEKRAR gönderilebilir. Sessiz kalmamalı — "iki kez
		// gönderilmedi" garantisinin tek zayıf noktası bu ve nadir.
		d.logger.Error("bildirim gönderildi ama deftere yazılamadı",
			slog.String("delivery_id", row.ID.String()),
			slog.String("error", err.Error()))
	}

	return true
}

func (d *Dispatcher) markFailed(ctx context.Context, row store.ClaimPendingDeliveriesRow, cause error) {
	message := cause.Error()

	d.logger.Warn("bildirim gönderilemedi",
		slog.String("delivery_id", row.ID.String()),
		slog.String("kind", string(row.ChannelKind)),
		slog.Int("attempts", int(row.Attempts)+1),
		slog.String("error", message))

	if err := d.queries.MarkDeliveryFailed(ctx, store.MarkDeliveryFailedParams{
		ID:        row.ID,
		LastError: &message,
		//nolint:gosec // MaxAttempts yapılandırmadan doğrulanarak geliyor
		MaxAttempts: int32(d.opts.MaxAttempts),
		Backoff:     intervalOf(d.opts.Backoff),
	}); err != nil {
		d.logger.Error("başarısız bildirim deftere yazılamadı",
			slog.String("delivery_id", row.ID.String()),
			slog.String("error", err.Error()))
	}
}

// messageOf, defter satırını gönderilecek mesaja çevirir.
func messageOf(row store.ClaimPendingDeliveriesRow) Message {
	return Message{
		Event:      row.Event,
		OrgName:    row.OrgName,
		Title:      row.IncidentTitle,
		Impact:     row.IncidentImpact,
		StartedAt:  row.IncidentStartedAt,
		ResolvedAt: row.IncidentResolvedAt,
	}
}

// intervalOf, Go süresini Postgres interval'ine çevirir.
func intervalOf(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
