package notifications

import (
	"context"
	"log/slog"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// MOCK E-POSTA ADAPTER'I (6.3, 8.2'de posta kutusu eklendi).
//
// Gönderim LOG'A yazılıyor. Bu, projenin "ücretli/hesap gerektiren dış servis
// yok" kısıtının (docs/0.0-constraints.md §2) doğrudan sonucu: depoyu
// klonlayan biri hiçbir API anahtarı olmadan akışın tamamını çalıştırabilmeli.
//
// Log satırı BİLİNÇLİ OLARAK gerçek gönderimin taşıyacağı alanları taşıyor
// (hedef, olay, başlık, etki). Böylece "bildirim gitti mi" sorusu geliştirme
// ortamında da cevaplanabiliyor ve gerçek sağlayıcıya geçildiğinde
// karşılaştırılacak bir referans var.

// MockEmailChannel, e-posta gönderimini loglayan ve (varsa) posta kutusuna
// yazan mock adapter.
//
// ADI 8.2'DE `LogEmailChannel`DAN DEĞİŞTİ: adapter artık yalnızca loglamıyor,
// gönderimi ayrıca saklıyor. Eski ad yanlış değildi ama EKSİKTİ ve bu projede
// adın davranışı tam anlatması yazılı bir disiplin — 7.4, hiçbir yerden
// çağrılmayan `LogWebhookChannel`ı tam bu gerekçeyle silmişti ("kodu okuyan
// kişi sistemin yanlış modelini kurar").
type MockEmailChannel struct {
	logger *slog.Logger
	// outbox nil olabilir: "posta kutusu kapalı" (üretim). Bkz. outbox.go.
	outbox *Outbox
}

// NewMockEmailChannel, mock e-posta adapter'ını kurar.
//
// `outbox` nil geçilebilir ve o hâlde adapter 6.3'teki davranışının birebir
// aynısını gösteriyor (yalnızca log).
func NewMockEmailChannel(outbox *Outbox, logger *slog.Logger) *MockEmailChannel {
	if logger == nil {
		logger = slog.Default()
	}

	return &MockEmailChannel{logger: logger, outbox: outbox}
}

// Kind, adapter'ın karşıladığı tür.
func (c *MockEmailChannel) Kind() store.NotificationChannelKind {
	return store.NotificationChannelKindEmail
}

// Mock, bu adapter'ın dışarıya HİÇBİR ŞEY göndermediğini bildirir.
//
// Kayıt (Registry) bunu üretimde bir açılış uyarısına çeviriyor: sessizce
// çalışan bir mock, "bildirim gitti" diyen bir defter ve hiç e-posta almayan
// bir kullanıcı demek.
func (c *MockEmailChannel) Mock() bool { return true }

// Send, e-postayı "gönderir" (log'a yazar ve posta kutusuna kaydeder).
func (c *MockEmailChannel) Send(ctx context.Context, target string, msg Message) error {
	// KONU SATIRI BURADA ÜRETİLİYOR, Message'da değil: "subject" bir e-posta
	// kavramı ve webhook'ta karşılığı yok. Biçimi bilen taraf adapter.
	c.logger.Info("e-posta bildirimi gönderildi (mock)",
		slog.String("to", target),
		slog.String("subject", subjectFor(msg)),
		slog.String("event", string(msg.Event)),
		slog.String("incident", msg.Title),
		slog.String("impact", string(msg.Impact)),
		slog.String("org", msg.OrgName),
		slog.String("body", msg.Body))

	// POSTA KUTUSU YAZILAMAZSA GÖNDERİM YİNE DE BAŞARILI (8.2).
	//
	// Posta kutusu bir GELİŞTİRME ARACI; hata döndürmek, onun arızasını
	// ürünün arızasına çevirmek olurdu: 6.4'ün abonelik ucu 500 döner, 8.3'ün
	// daveti yazılmaz. Kaybedilen bilgi de yok — gönderimin kaydı yukarıdaki
	// LOG SATIRINDA duruyor ve 8.2'den önce zaten tek kayıt oydu. Sessiz de
	// kalmıyor: ERROR seviyesinde loglanıyor, çünkü boş bir posta kutusu
	// ekranı "hiç e-posta gönderilmedi" gibi okunur.
	if err := c.outbox.record(ctx, target, msg); err != nil {
		c.logger.Error("posta kutusuna yazılamadı (gönderim başarılı sayıldı)",
			slog.String("to", target),
			slog.String("error", err.Error()))
	}

	return nil
}

// WEBHOOK'UN MOCK ADAPTER'I 7.4'TE SİLİNDİ. 6.3 burada istek atmayan bir
// `LogWebhookChannel` bırakmıştı ("gerçek göndericiyi eklemek korumayı da
// eklemek demek"); 7.4 o göndericiyi `webhook.go`daki
// `HTTPWebhookChannel` ile yazdı ve `NewRegistry` o günden beri gerçek olanı
// kuruyor. Mock'u "desen dursun" diye bırakmak, hiçbir yerden çağrılmayan ama
// dokümantasyonu hâlâ "istek atılmıyor" diyen bir dosya bırakmak olurdu —
// kodu okuyan kişi sistemin yanlış modelini kurar.
//
// E-posta tarafındaki mock DURUYOR ve sebebi farklı: orada gerçek gönderim
// bir sağlayıcı hesabı ister (GÜNCEL DURUM §2), webhook'un karşı tarafı ise
// kullanıcının KENDİ sunucusu.

// subjectFor, e-posta konu satırı.
//
// TÜRE GÖRE DALLANIYOR (8.3'ün düzeltmesi): işlem e-postaları (abonelik onayı,
// davet, parola sıfırlama) kesinti değil, yani konuya "Kesinti:" öneki
// KONMUYOR. Önceki hâlde bir davet e-postası "[Acme] Kesinti: Organizasyona
// davet edildiniz" başlığıyla gidiyordu.
func subjectFor(msg Message) string {
	if msg.Kind == MessageKindTransactional {
		return "[" + msg.OrgName + "] " + msg.Title
	}

	if msg.Event == store.NotificationEventIncidentResolved {
		return "[" + msg.OrgName + "] Çözüldü: " + msg.Title
	}

	return "[" + msg.OrgName + "] Kesinti: " + msg.Title
}
