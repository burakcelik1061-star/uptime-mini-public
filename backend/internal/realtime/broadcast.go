package realtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Canlı durum güncellemeleri (Faz 6.2).
//
// OLAYLAR GÖVDESİZ — bilinçli ve bu adımın en önemli kararı.
//
// Bir olay yalnızca "bu org'un (veya bu public sayfanın) verisi değişti" diyor;
// yeni durumu TAŞIMIYOR. İstemci sinyali alınca zaten kullandığı uçtan veriyi
// yeniden okuyor. Üç sebebi var ve üçü de bu projede yazılı:
//
//  1. AYRIŞMA İMKÂNSIZ. 3.3/4.1'in kuralı "durum değişimi tek bir yerden
//     geçer" idi; gövdeli bir olay, panonun okuduğu cevabın İKİNCİ bir kopyası
//     olurdu ve iki kopya er geç ayrışırdı (canlı yayın bir şey, sayfa başka
//     bir şey söylerdi).
//  2. SIZINTI KURALI TEK YERDE KALIYOR. 5.1'in kuralı public sorguların
//     İÇİNDE yazılı; gövdesiz olay o sorguların dışına hiçbir alan taşımıyor,
//     yani kuralı ikinci kez uygulamak gerekmiyor.
//  3. KOPUKKEN KAÇIRILAN OLAY SORUNU KENDİLİĞİNDEN ÇÖZÜLÜYOR. İstemcinin her
//     olaya verdiği cevap "tazele" olduğu için, yeniden bağlandığında
//     kaçırdığı olayları oynatmasına gerek yok: bağlantı kurulduğunu bildiren
//     `realtime.ready` olayı da bir tazeleme sebebidir. Olay tamponu, "since"
//     imleci ve tekrar oynatma yok.
//
// `reason` yalnızca LOG ve tanı içindir; istemci ona bakarak farklı davranmıyor
// (bakarsa 1. madde geri gelir).
const (
	// EventStatusChanged, "veri değişti, tazele" sinyali.
	EventStatusChanged = "status.changed"
)

// Değişikliğin sebepleri (yalnızca tanı için).
const (
	ReasonMonitorStatus   = "monitor.status_changed"
	ReasonIncidentOpened  = "incident.opened"
	ReasonIncidentClosed  = "incident.resolved"
	ReasonIncidentCreated = "incident.created"
	ReasonIncidentUpdated = "incident.updated"
)

// PublicResolver, değişikliğin PUBLIC kanala çıkıp çıkmayacağını söyleyen
// bağımlılık (internal/statuspage → PublicSlugFor).
//
// Arayüz olarak duruyor çünkü kuralın kendisi bu pakete AİT DEĞİL: burası
// taşıma katmanı, "hangi servis yayımlanmış" sorusunun cevabı 5.1'in
// sorgularında.
type PublicResolver interface {
	PublicSlugFor(ctx context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID) (string, bool, error)
}

// PublicChannel, bir status sayfasının OTURUMSUZ kanalının adı.
//
// Kanal adı SLUG'a bağlı, org kimliğine değil: ziyaretçinin elinde yalnızca
// slug var (adres çubuğunda o yazıyor) ve org kimliğini dışarıya vermek için
// bir sebep yok.
func PublicChannel(slug string) string {
	return "public:" + slug
}

// Broadcaster, bir değişikliği ilgili kanallara duyurur.
type Broadcaster struct {
	notifier *Notifier
	public   PublicResolver
	logger   *slog.Logger
	now      func() time.Time
}

// NewBroadcaster, yayıncıyı kurar.
func NewBroadcaster(notifier *Notifier, public PublicResolver, logger *slog.Logger) *Broadcaster {
	if logger == nil {
		logger = slog.Default()
	}

	return &Broadcaster{notifier: notifier, public: public, logger: logger, now: time.Now}
}

// Changed, bir değişikliği duyurur: her zaman org kanalına, kapıdan geçerse
// public kanala da.
//
// HATA DÖNDÜRMÜYOR ve çağıranı BLOKE ETMEMELİ. Yayın, veriyi yazan işin
// başarısının bir parçası değil: bir kesinti kaydı yazıldıysa yazılmıştır,
// duyurusunun kaybolması panonun bir sonraki okumada doğruyu görmesini
// engellemez (4.1'in `applyIncident` disipliniyle aynı). Bu yüzden hatalar
// yalnızca loglanıyor.
func (b *Broadcaster) Changed(ctx context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID, reason string) {
	event, err := NewEvent(EventStatusChanged, b.now(), map[string]string{"reason": reason})
	if err != nil {
		b.logger.Error("canlı olay üretilemedi",
			slog.String("reason", reason),
			slog.String("error", err.Error()))

		return
	}

	// ORG KANALI: değişikliğin türü ne olursa olsun org'un panosu tazelenmeli.
	if err := b.notifier.Publish(ctx, OrgChannel(orgID), event); err != nil {
		b.logger.Warn("org kanalına yayın yapılamadı",
			slog.String("org_id", orgID.String()),
			slog.String("reason", reason),
			slog.String("error", err.Error()))
	}

	slug, ok, err := b.public.PublicSlugFor(ctx, orgID, monitorIDs)
	if err != nil {
		b.logger.Warn("public kanal çözümlenemedi",
			slog.String("org_id", orgID.String()),
			slog.String("reason", reason),
			slog.String("error", err.Error()))

		return
	}

	if !ok {
		// Yayımlanmamış sayfa ya da yalnızca private servisleri ilgilendiren
		// bir değişiklik: public kanala HİÇBİR ŞEY çıkmıyor.
		return
	}

	if err := b.notifier.Publish(ctx, PublicChannel(slug), event); err != nil {
		b.logger.Warn("public kanala yayın yapılamadı",
			slog.String("slug", slug),
			slog.String("reason", reason),
			slog.String("error", err.Error()))
	}
}
