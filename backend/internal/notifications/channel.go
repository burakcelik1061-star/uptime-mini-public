// Package notifications, kesinti bildirimlerini yönetir (Faz 6.3):
// organizasyonun bildirim kanalları (yapılandırma) ve o kanallara yapılan
// gönderimler (defter + gönderici).
//
// GERÇEK SAĞLAYICI YOK (docs/0.0-constraints.md §2): e-posta ve webhook
// adapter'ları bugün log'a yazıyor. Bu bir "sonra doldururuz" boşluğu değil,
// bilinçli bir sınır — ve maliyeti tek bir yere hapsedilmiş durumda:
// `NewRegistry`. Gerçek bir sağlayıcı eklemek, o yapıcı fonksiyonda tek bir
// satırın değişmesi demek; çağıran hiçbir kod (job, handler, testler)
// değişmiyor.
package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Message, tek bir bildirimin İÇERİĞİ — taşıma biçiminden bağımsız.
//
// Adapter'lar bu yapıyı kendi biçimlerine çeviriyor (e-posta gövdesi, webhook
// JSON'ı). Metnin kendisi burada ÜRETİLMİYOR: "konu satırı" bir e-posta
// kavramı, webhook'un öyle bir alanı yok.
// MessageKind, mesajın NE HAKKINDA olduğu (8.3'te eklendi).
//
// NEDEN GEREKTİ: 6.3 `Message`ı kesinti bildirimi için tasarlamıştı ve konu
// satırını `Event`ten türetiyordu. 6.4 çift onay e-postasını, 8.3 de daveti
// aynı yapıyla taşıyınca (`Event: incident_opened` geçerek) konu satırı
// "[Acme] KESİNTİ: Aboneliğinizi onaylayın" oluyordu — yani bir onay
// e-postası kesinti gibi görünüyordu. Hata 8.2'ye kadar görünmedi çünkü
// gönderim yalnızca loga yazılıyordu; posta kutusu ekranı onu ortaya çıkardı.
//
// Çözüm konu satırını ÇAĞIRANA yazdırmak DEĞİL: 6.3'ün "biçime özgü şeyleri
// adapter üretir" kuralı duruyor ("subject" bir e-posta kavramı, webhook'un
// öyle bir alanı yok). `Message` yalnızca ANLAM taşıyor, biçim taşımıyor.
type MessageKind string

const (
	// MessageKindIncident, kesinti açılış/kapanış bildirimi. SIFIR DEĞER
	// bilinçli olarak bu: 6.3'ün gönderim job'ı hiç değişmeden aynı davranışı
	// sürdürüyor.
	MessageKindIncident MessageKind = ""
	// MessageKindTransactional, kişiye özel işlem e-postası (abonelik onayı,
	// davet, parola sıfırlama). Kesinti alanları boş.
	MessageKindTransactional MessageKind = "transactional"
)

type Message struct {
	// Kind, mesajın türü. Boş bırakılırsa kesinti bildirimi sayılıyor.
	Kind       MessageKind
	Event      store.NotificationEvent
	OrgName    string
	Title      string
	Impact     store.IncidentImpact
	StartedAt  time.Time
	ResolvedAt *time.Time
	// Body, mesajın gövdesi. Kesinti bildirimlerinde boş (adapter gövdeyi
	// alanlardan üretiyor); 6.4'ün onay/iptal e-postalarında ise linkleri
	// taşıyor — o metni üreten taraf abonelik servisi, çünkü linkler ARAYÜZE
	// işaret ediyor ve adresini yalnızca o biliyor.
	Body string
}

// Channel, bir bildirim taşıyıcısı. 6.3'ün istediği soyutlama bu.
//
// `target` mesajın parçası DEĞİL, ayrı bir parametre: aynı mesaj birden çok
// hedefe gidebiliyor ve adapter'ın hedefi yorumlaması (e-posta adresi mi,
// URL mi) türe özgü.
type Channel interface {
	// Kind, bu adapter'ın karşıladığı kanal türü.
	Kind() store.NotificationChannelKind
	// Send, mesajı hedefe iletir. Hata dönerse gönderim yeniden denenecek
	// (bkz. deliveries.go).
	Send(ctx context.Context, target string, msg Message) error
}

// Registry, kanal türünden adapter'a eşleme.
type Registry struct {
	channels map[store.NotificationChannelKind]Channel
}

// NewRegistry, KULLANILACAK ADAPTER'LARI SEÇEN TEK YER.
//
// Gerçek bir sağlayıcıya geçiş burada olur ve YALNIZCA burada:
//
//	e-posta:  NewMockEmailChannel(outbox, logger)  →  NewResendChannel(apiKey, logger)
//	webhook:  NewHTTPWebhookChannel(policy, logger)   ← 7.4'te GERÇEK OLDU
//
// Gönderim job'ı, handler'lar ve testler adapter'ın hangisi olduğunu bilmiyor.
// canny-mini'deki `EmailProvider` deseninin Go karşılığı: arayüz + tek bir
// kurulum noktası.
//
// WEBHOOK ARTIK GERÇEKTEN GÖNDERİYOR (7.4), E-POSTA HÂLÂ MOCK. Ayrım
// keyfi değil: webhook'un karşı tarafı KULLANICININ KENDİ sunucusu, yani
// hiçbir sağlayıcı hesabı gerekmiyor ve "klonlayan herkes çalıştırabilmeli"
// sözü (GÜNCEL DURUM §2/§3) bozulmuyor. E-posta ise bir sağlayıcı hesabı
// isterdi. Gerçek göndericiyi eklemek, SSRF korumasını da eklemek demekti —
// `policy` bu yüzden argüman.
//
// `outbox` NIL OLABİLİR ve nil olması "posta kutusu kapalı" demek (8.2).
// Kapının kendisi burada DEĞİL, çağıranda: `config` APP_ENV açıkça
// development|test iken bir posta kutusu kuruyor, aksi hâlde nil geçiyor
// (7.5'in envExplicit disiplini). Kapıyı buraya koymak, gerçek sağlayıcıya
// geçildiğinde anlamsız kalacak bir ortam kontrolünü adapter seçiminin
// içine gömmek olurdu.
func NewRegistry(policy safedial.Policy, outbox *Outbox, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}

	return NewRegistryWith(
		NewMockEmailChannel(outbox, logger),
		NewHTTPWebhookChannel(policy, logger),
	)
}

// NewRegistryWith, verilen adapter'larla bir kayıt kurar (testler ve ileride
// yapılandırmaya göre seçim için).
func NewRegistryWith(channels ...Channel) *Registry {
	registry := &Registry{channels: make(map[store.NotificationChannelKind]Channel, len(channels))}
	for _, channel := range channels {
		registry.channels[channel.Kind()] = channel
	}

	return registry
}

// Mocked, bir adapter'ın GERÇEKTEN göndermediğini bildirir.
//
// İsteğe bağlı bir arayüz: uygulamayan adapter gerçek sayılıyor. Karşılığı bir
// bayrak değil bir DAVRANIŞ sorusu — "bu adapter dışarıya çıkıyor mu" — ve
// cevabı yalnızca adapter'ın kendisi biliyor.
type Mocked interface {
	// Mock true ise gönderim yalnızca loglanıyor.
	Mock() bool
}

// MockWarnings, üretimde mock bir adapter'ın etkin olduğunu bildiren uyarılar
// döner (7.5).
//
// ASIL RİSK LOGDAKİ TOKEN DEĞİL, ÜRETİMDE MOCK'UN ÇALIŞIYOR OLMASI. Mock
// e-posta adapter'ı bir sağlayıcı değil bir GELİŞTİRME ARACI: gönderimi
// (onay/iptal linkleri dahil) loga yazıyor ve bu kurulumda logun kendisi posta
// kutusu — 6.4'ün akışını klonlayan biri ancak böyle deneyebiliyor. Ama aynı
// adapter üretimde çalışırsa kimse bir e-posta ALMIYOR ve bunu kimse fark
// etmiyor: gönderim defterinde satırlar `sent` yazmaya devam ediyor.
//
// Gerçek sağlayıcıya geçiş tek bir yapıcıda (NewRegistry, 6.3). Bu uyarı da
// tam olarak oraya işaret ediyor. Ortama göre DALLANAN bir davranış eklenmedi:
// bu projede üretim ortamı yok (GÜNCEL DURUM §1), yani hiç koşmayan bir kod
// yolu ve yalnızca kendisi için yazılmış bir test doğardı — üstelik
// "üretimde maskeliyoruz" demek mock'u üretime uygunmuş gibi gösterirdi.
func (r *Registry) MockWarnings(isProduction bool) []string {
	if !isProduction {
		return nil
	}

	// Sıra sabit: uyarı satırlarının her açılışta aynı sırada çıkması,
	// logları karşılaştıran birinin işini kolaylaştırıyor.
	kinds := []store.NotificationChannelKind{
		store.NotificationChannelKindEmail,
		store.NotificationChannelKindWebhook,
	}

	var warnings []string
	for _, kind := range kinds {
		channel, ok := r.channels[kind]
		if !ok {
			continue
		}

		if mock, isMock := channel.(Mocked); isMock && mock.Mock() {
			warnings = append(warnings, fmt.Sprintf(
				"%q bildirim kanalı MOCK adapter kullanıyor: gerçek gönderim yapılmıyor, "+
					"gönderimler yalnızca loga yazılıyor (gerçek sağlayıcı: internal/notifications → NewRegistry)",
				kind))
		}
	}

	return warnings
}

// For, tür için adapter döner.
func (r *Registry) For(kind store.NotificationChannelKind) (Channel, error) {
	channel, ok := r.channels[kind]
	if !ok {
		// Şemadaki enum ile buradaki kayıt ayrışırsa sessiz kalmamalı: o kanala
		// yapılan her gönderim sonsuza kadar başarısız olurdu.
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, kind)
	}

	return channel, nil
}
