package subscribers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/worker"
)

// Abonelere kesinti bildirimi (Faz 6.4).
//
// İSKELET 6.3'ÜN AYNISI ve bu bilinçli: tara → kuyruğa yaz → sahiplen →
// gönder. Aynı garantiler geçerli — gönderim kesinti yazmayı bloke etmiyor
// (kuyruğa yazma da incident yazma yolunda değil) ve "iki kez göndermeme"
// ŞEMADA (`(subscriber_id, incident_id, event)` UNIQUE).
//
// TEK GERÇEK FARK: ZİYARETÇİYE GİDEN BİR BİLDİRİM, 5.1'İN SIZINTI KURALINA
// TABİ. Yayımlanmamış bir servisin kesintisi aboneye gitmemeli — abonenin
// gördüğü, sayfada göreceğinden fazlası olamaz. Kural BURAYA YENİDEN
// YAZILMIYOR: karar 5.1'in kendi sorgularına soruluyor (`PublicGate`), tıpkı
// 6.2'nin public kanal kapısı gibi.

// JobName, runtime'da görünen job adı.
const JobName = "subscriber-notifications"

const (
	defaultLookback    = 24 * time.Hour
	defaultBatchSize   = 20
	defaultMaxAttempts = 3
	defaultBackoff     = time.Minute

	// candidateLimit, bir turda görünürlük kapısına sorulacak en fazla olay.
	candidateLimit = 100

	claimLease  = 5 * time.Minute
	sendTimeout = 10 * time.Second
)

// PublicGate, bir kesintinin ziyaretçiye görünür olup olmadığını söyleyen
// bağımlılık (internal/statuspage → PublicSlugFor).
//
// 6.2'nin canlı yayın kapısıyla AYNI fonksiyon: "yayımlanan servis" tanımının
// tek bir kopyası var ve o kopya 5.1'in sorgularında.
type PublicGate interface {
	PublicSlugFor(ctx context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID) (string, bool, error)
}

// DeliveryStore, gönderim job'ının ihtiyaç duyduğu sorgular.
type DeliveryStore interface {
	ListSubscribableIncidentEvents(
		ctx context.Context, arg store.ListSubscribableIncidentEventsParams,
	) ([]store.ListSubscribableIncidentEventsRow, error)
	ListIncidentMonitorIDs(ctx context.Context, incidentID uuid.UUID) ([]uuid.UUID, error)
	EnqueueSubscriberNotifications(
		ctx context.Context, arg store.EnqueueSubscriberNotificationsParams,
	) (int64, error)
	ClaimPendingSubscriberDeliveries(
		ctx context.Context, arg store.ClaimPendingSubscriberDeliveriesParams,
	) ([]store.ClaimPendingSubscriberDeliveriesRow, error)
	MarkSubscriberDeliverySent(ctx context.Context, id uuid.UUID) error
	MarkSubscriberDeliveryFailed(ctx context.Context, arg store.MarkSubscriberDeliveryFailedParams) error
}

// Dispatcher, abonelere bekleyen bildirimleri gönderir.
type Dispatcher struct {
	queries DeliveryStore
	gate    PublicGate
	mailer  Mailer
	logger  *slog.Logger
	// publicBaseURL, e-postadaki iptal linkinin kökü.
	publicBaseURL string
}

// NewDispatcher, göndericiyi kurar.
func NewDispatcher(
	queries DeliveryStore, gate PublicGate, mailer Mailer, publicBaseURL string, logger *slog.Logger,
) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}

	return &Dispatcher{
		queries:       queries,
		gate:          gate,
		mailer:        mailer,
		logger:        logger,
		publicBaseURL: publicBaseURL,
	}
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
	Enqueued int64
	Sent     int
	Failed   int
	// Hidden, sızıntı kuralı gereği ATLANAN olay sayısı.
	Hidden int
}

// RunOnce, bir tur çalışır.
func (d *Dispatcher) RunOnce(ctx context.Context) (DispatchStats, error) {
	var stats DispatchStats

	candidates, err := d.queries.ListSubscribableIncidentEvents(ctx, store.ListSubscribableIncidentEventsParams{
		Lookback: intervalOf(defaultLookback),
		RowLimit: candidateLimit,
	})
	if err != nil {
		return stats, fmt.Errorf("abone bildirimi adayları okunamadı: %w", err)
	}

	for _, candidate := range candidates {
		enqueued, visible, err := d.enqueue(ctx, candidate)
		if err != nil {
			return stats, err
		}

		if !visible {
			stats.Hidden++

			continue
		}

		stats.Enqueued += enqueued
	}

	claimed, err := d.queries.ClaimPendingSubscriberDeliveries(ctx, store.ClaimPendingSubscriberDeliveriesParams{
		RowLimit:   defaultBatchSize,
		ClaimLease: intervalOf(claimLease),
	})
	if err != nil {
		return stats, fmt.Errorf("bekleyen abone bildirimleri sahiplenilemedi: %w", err)
	}

	for _, delivery := range claimed {
		if d.deliver(ctx, delivery) {
			stats.Sent++
		} else {
			stats.Failed++
		}
	}

	if stats.Enqueued > 0 || stats.Sent > 0 || stats.Failed > 0 {
		d.logger.Info("abone bildirim turu tamamlandı",
			slog.Int64("enqueued", stats.Enqueued),
			slog.Int("sent", stats.Sent),
			slog.Int("failed", stats.Failed),
			slog.Int("hidden", stats.Hidden))
	}

	return stats, nil
}

// enqueue, tek bir olayı GÖRÜNÜRLÜK KAPISINDAN geçirip kuyruğa yazar.
func (d *Dispatcher) enqueue(
	ctx context.Context, candidate store.ListSubscribableIncidentEventsRow,
) (int64, bool, error) {
	monitorIDs, err := d.queries.ListIncidentMonitorIDs(ctx, candidate.IncidentID)
	if err != nil {
		return 0, false, fmt.Errorf("kesintinin servisleri okunamadı: %w", err)
	}

	// KAPI 5.1'İN KENDİ SORGULARINA SORUYOR. Kural burada tekrar edilmiyor;
	// tekrar edilseydi, sızıntı iki kopyanın ayrıştığı gün doğardı.
	_, visible, err := d.gate.PublicSlugFor(ctx, candidate.OrgID, monitorIDs)
	if err != nil {
		return 0, false, fmt.Errorf("kesintinin görünürlüğü çözümlenemedi: %w", err)
	}

	if !visible {
		// Yayımlanmamış bir servisin kesintisi aboneye GİTMİYOR: abonenin
		// gördüğü, sayfada göreceğinden fazlası olamaz.
		return 0, false, nil
	}

	enqueued, err := d.queries.EnqueueSubscriberNotifications(ctx, store.EnqueueSubscriberNotificationsParams{
		StatusPageID: candidate.StatusPageID,
		IncidentID:   candidate.IncidentID,
		Event:        candidate.Event,
		OccurredAt:   &candidate.OccurredAt,
	})
	if err != nil {
		return 0, false, fmt.Errorf("abone bildirimleri kuyruğa alınamadı: %w", err)
	}

	return enqueued, true, nil
}

// deliver, tek bir aboneye gönderim yapar.
func (d *Dispatcher) deliver(ctx context.Context, row store.ClaimPendingSubscriberDeliveriesRow) bool {
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	// HER BİLDİRİMDE ÇALIŞAN BİR İPTAL LİNKİ VAR: aboneliğin nasıl biteceğini
	// söylemeyen bir bildirim spam'in tanımı. Linki her seferinde
	// üretebiliyor olmak, iptal token'ının ham saklanma kararının tam olarak
	// sebebi (bkz. migration 00017).
	body := fmt.Sprintf(
		"%s durumundaki değişiklik. Sayfa: %s\nAbonelikten çıkmak için: %s",
		row.PageTitle,
		d.publicBaseURL+"/status/"+row.PageSlug,
		d.publicBaseURL+"/status/"+row.PageSlug+"/unsubscribe?token="+row.UnsubscribeToken,
	)

	msg := notifications.Message{
		Event:      row.Event,
		OrgName:    row.PageTitle,
		Title:      row.IncidentTitle,
		Impact:     row.IncidentImpact,
		StartedAt:  row.IncidentStartedAt,
		ResolvedAt: row.IncidentResolvedAt,
		Body:       body,
	}

	if err := d.mailer.Send(sendCtx, row.Email, msg); err != nil {
		message := err.Error()

		d.logger.Warn("abone bildirimi gönderilemedi",
			slog.String("delivery_id", row.ID.String()),
			slog.Int("attempts", int(row.Attempts)+1),
			slog.String("error", message))

		if err := d.queries.MarkSubscriberDeliveryFailed(ctx, store.MarkSubscriberDeliveryFailedParams{
			ID:          row.ID,
			LastError:   &message,
			MaxAttempts: defaultMaxAttempts,
			Backoff:     intervalOf(defaultBackoff),
		}); err != nil {
			d.logger.Error("başarısız abone bildirimi deftere yazılamadı",
				slog.String("delivery_id", row.ID.String()),
				slog.String("error", err.Error()))
		}

		return false
	}

	if err := d.queries.MarkSubscriberDeliverySent(ctx, row.ID); err != nil {
		d.logger.Error("abone bildirimi gönderildi ama deftere yazılamadı",
			slog.String("delivery_id", row.ID.String()),
			slog.String("error", err.Error()))
	}

	return true
}

// intervalOf, Go süresini Postgres interval'ine çevirir.
func intervalOf(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
