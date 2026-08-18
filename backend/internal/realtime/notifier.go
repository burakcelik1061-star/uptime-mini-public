package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrPayloadTooLarge, olay Postgres'in NOTIFY sınırını aştığında döner.
var ErrPayloadTooLarge = errors.New("realtime: olay payload'ı çok büyük")

// Store, yayıncının veritabanından ihtiyaç duyduğu sorgu.
type Store interface {
	PublishRealtimeEvent(ctx context.Context, arg store.PublishRealtimeEventParams) error
}

// Notifier, olayları süreçler arası taşıya (Postgres NOTIFY) verir.
//
// Kullanan taraf worker'dır (olay orada doğuyor) ama api de kullanabilir:
// api'nin kendi ürettiği bir olayı doğrudan Hub'a vermek yerine aynı yoldan
// geçirmek, çok api kopyalı bir kurulumda olayın DİĞER kopyalara bağlı
// istemcilere de ulaşmasını sağlıyor. Yani taşıma yolu tek: "olay hub'a
// yalnızca dinleyiciden girer".
type Notifier struct {
	queries Store
	logger  *slog.Logger
}

// NewNotifier, yayıncıyı kurar.
func NewNotifier(queries Store, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}

	return &Notifier{queries: queries, logger: logger}
}

// Publish, olayı verilen kanala yayımlar.
//
// TRANSACTION İÇİNDE ÇAĞRILABİLİR ve çağrılmalıdır: `pg_notify` bildirimi
// COMMIT anında gönderiyor, yani transaction geri alınırsa olay da hiç
// doğmuyor. "Önce yaz, commit et, sonra yayımla" sıralaması ise commit ile
// yayım arasında düşen bir süreçte olayı sessizce kaybederdi.
func (n *Notifier) Publish(ctx context.Context, channel string, event Event) error {
	payload, err := json.Marshal(envelope{Channel: channel, Event: event})
	if err != nil {
		return fmt.Errorf("olay kodlanamadı: %w", err)
	}

	if len(payload) > maxNotifyPayload {
		// Sınır BURADA da duruyor: veritabanına bırakılsaydı hata
		// "22023: payload string too long" olarak görünür, sebebi (olay gövdesi
		// büyümüş) log satırından anlaşılmazdı.
		return fmt.Errorf("%w: %d bayt (üst sınır %d)", ErrPayloadTooLarge, len(payload), maxNotifyPayload)
	}

	if err := n.queries.PublishRealtimeEvent(ctx, store.PublishRealtimeEventParams{
		Channel: NotifyChannel,
		Payload: string(payload),
	}); err != nil {
		return fmt.Errorf("olay yayımlanamadı: %w", err)
	}

	n.logger.Debug("canlı olay yayımlandı",
		slog.String("channel", channel),
		slog.String("event", event.Type),
		slog.Int("bytes", len(payload)))

	return nil
}
