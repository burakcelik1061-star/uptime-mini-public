// Package billing, plan yükseltmesinin ödeme adımını soyutlar (Faz 8.5).
//
// ÖDEME MOCK ve bu bir eksiklik değil, docs/0.0-constraints.md §2'nin
// doğrudan sonucu: bu projede ücretli/hesap gerektiren dış servis yok. Mock,
// akışın ÇALIŞTIĞINI göstermenin yolu — 8.5'in demo değeri "duvarı görmek"
// değil, "duvarı aşmanın işe yaradığını görmek".
//
// DESEN 6.3'ÜN `notifications.NewRegistry`SİNİN AYNISI ve bilinçli olarak
// ikinci bir soyutlama katmanı yazılmadı:
//
//   - `Provider` bir ARAYÜZ,
//   - gerçek sağlayıcıya geçiş TEK BİR YAPICI FONKSİYONDA (`NewProvider`),
//   - çağıran (servis, handler, testler) hangi uygulamanın kullanıldığını
//     BİLMİYOR,
//   - mock olduğunu adapter'ın kendisi söylüyor (`Mock()`), üretimde açılış
//     uyarısına çevriliyor (7.5'in mock e-posta uyarısıyla aynı).
//
// SDK EKLENMEDİ, hesap açılmadı: gerçek bir sağlayıcı eklemek bu dosyada tek
// satırlık bir değişiklik olacak şekilde duruyor.
package billing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Charge, bir plan yükseltmesi için ödeme isteği.
type Charge struct {
	OrgID   uuid.UUID
	OrgName string
	Plan    store.OrgPlan
}

// Receipt, ödemenin sonucu.
//
// TUTAR YOK ve bu bilinçli: fiyatlandırma bu projenin kapsamı dışında (0.1 §3
// billing'i MVP dışına aldı). Buradaki iş "ödeme adımı nereye takılacak"
// sorusunun cevabını yapıya bağlamak; bir para birimi/tutar alanı, hiçbir
// yerde kullanılmayan ve ilk gerçek sağlayıcıda zaten değişecek bir alan
// olurdu (2.1'in "sütun, yazma yoluna sahip olan adımda gelir" kuralı).
type Receipt struct {
	// Reference, sağlayıcının işlem kimliği. Mock'ta üretilen bir uuid.
	Reference string
	// PaidAt, ödemenin gerçekleştiği an.
	PaidAt time.Time
}

// Provider, ödeme sağlayıcısı.
type Provider interface {
	// Charge, yükseltmenin ödemesini alır.
	Charge(ctx context.Context, charge Charge) (Receipt, error)
}

// Mocked, sağlayıcının GERÇEKTEN tahsilat yapmadığını bildirir.
//
// 6.3'ün `notifications.Mocked` arayüzüyle aynı: uygulamayan sağlayıcı gerçek
// sayılıyor ve cevabı bir bayrak değil bir DAVRANIŞ sorusu — "bu adapter
// dışarıya çıkıyor mu".
type Mocked interface {
	Mock() bool
}

// NewProvider, KULLANILACAK SAĞLAYICIYI SEÇEN TEK YER.
//
// Gerçek bir sağlayıcıya geçiş burada olur ve YALNIZCA burada:
//
//	NewMockProvider(logger)  →  NewStripeProvider(apiKey, logger)
func NewProvider(logger *slog.Logger) Provider {
	return NewMockProvider(logger)
}

// MockProvider, hiçbir yere para göndermeyen sağlayıcı.
type MockProvider struct{ logger *slog.Logger }

// NewMockProvider, mock sağlayıcıyı kurar.
func NewMockProvider(logger *slog.Logger) *MockProvider {
	if logger == nil {
		logger = slog.Default()
	}

	return &MockProvider{logger: logger}
}

// Mock, bu sağlayıcının dışarıya HİÇBİR ŞEY göndermediğini bildirir.
func (p *MockProvider) Mock() bool { return true }

// Charge, ödemeyi "alır" (loga yazar ve sahte bir dekont döner).
//
// HER ZAMAN BAŞARILI: başarısızlık simülasyonu eklenmedi çünkü test edilecek
// bir başarısızlık YOLU yok — gerçek sağlayıcı geldiğinde hataların şekli de
// onunla gelir ve şimdi uydurulan bir hata sınıflandırması ilk gün çöpe
// giderdi. Çağıran yine de hata döndürebileceğini varsayıyor (arayüz `error`
// döndürüyor) ve o yolu ele alıyor.
func (p *MockProvider) Charge(_ context.Context, charge Charge) (Receipt, error) {
	receipt := Receipt{
		Reference: "mock-" + uuid.NewString(),
		PaidAt:    time.Now(),
	}

	p.logger.Info("ödeme alındı (mock)",
		slog.String("org_id", charge.OrgID.String()),
		slog.String("org", charge.OrgName),
		slog.String("plan", string(charge.Plan)),
		slog.String("reference", receipt.Reference))

	return receipt, nil
}

// MockWarning, üretimde mock sağlayıcının etkin olduğunu bildiren uyarı (7.5).
//
// ASIL RİSK, ÜRETİMDE MOCK'UN ÇALIŞIYOR OLMASI: kimse ödeme yapmıyor ama
// planlar yükseliyor. Uyarı, 7.5'in mock e-posta uyarısıyla aynı desende ve
// gerçek sağlayıcının yerini (`NewProvider`) gösteriyor.
func MockWarning(provider Provider, isProduction bool) string {
	if !isProduction {
		return ""
	}

	mock, ok := provider.(Mocked)
	if !ok || !mock.Mock() {
		return ""
	}

	return fmt.Sprintf("ödeme sağlayıcısı MOCK: tahsilat YAPILMIYOR ama plan " +
		"yükseltmeleri uygulanıyor (gerçek sağlayıcı: internal/billing → NewProvider)")
}
