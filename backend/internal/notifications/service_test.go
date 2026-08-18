package notifications

import (
	"errors"
	"strings"
	"testing"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Doğrulama SERVİSTE, handler'da değil (Faz 2'den beri): kural türe göre
// değişiyor ve HTTP'yi tanımayan tek bir yerde durunca gerçek zamanlı bir
// istek olmadan ölçülebiliyor.

func TestValidateAcceptsValidChannels(t *testing.T) {
	t.Parallel()

	cases := []ChannelInput{
		{Kind: store.NotificationChannelKindEmail, Name: "Nöbetçi", Target: "nobetci@example.com"},
		{Kind: store.NotificationChannelKindWebhook, Name: "Slack köprüsü", Target: "https://ornek.example/hook"},
	}

	for _, in := range cases {
		if err := Validate(Normalize(in)); err != nil {
			t.Errorf("%s reddedildi: %v", in.Kind, err)
		}
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in    ChannelInput
		field string
	}{
		"tür yok": {
			in:    ChannelInput{Name: "Ad", Target: "a@b.com"},
			field: "kind",
		},
		"ad boş": {
			in:    ChannelInput{Kind: store.NotificationChannelKindEmail, Target: "a@b.com"},
			field: "name",
		},
		"ad çok uzun": {
			in: ChannelInput{
				Kind:   store.NotificationChannelKindEmail,
				Name:   strings.Repeat("a", maxChannelName+1),
				Target: "a@b.com",
			},
			field: "name",
		},
		"hedef boş": {
			in:    ChannelInput{Kind: store.NotificationChannelKindEmail, Name: "Ad"},
			field: "target",
		},
		"e-posta biçimsiz": {
			in:    ChannelInput{Kind: store.NotificationChannelKindEmail, Name: "Ad", Target: "bu-bir-adres-degil"},
			field: "target",
		},
		"e-posta alan adı noktasız": {
			in:    ChannelInput{Kind: store.NotificationChannelKindEmail, Name: "Ad", Target: "ali@localhost"},
			field: "target",
		},
		// http webhook: kesinti bildirimi organizasyonun iç durumunu taşıyor,
		// düz metin bir kanaldan gönderilmemeli (5.3'ün logo kararının aynısı).
		"webhook http": {
			in:    ChannelInput{Kind: store.NotificationChannelKindWebhook, Name: "Ad", Target: "http://ornek.example/h"},
			field: "target",
		},
		"webhook javascript şeması": {
			in:    ChannelInput{Kind: store.NotificationChannelKindWebhook, Name: "Ad", Target: "javascript:alert(1)"},
			field: "target",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := Validate(Normalize(tc.in))

			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("hata = %v, beklenen ValidationError", err)
			}

			for _, problem := range validation.Problems {
				if problem.Field == tc.field {
					return
				}
			}

			t.Fatalf("%q alanı için hata yok: %+v", tc.field, validation.Problems)
		})
	}
}

// Normalleştirme e-posta adresini küçük harfe indiriyor: aynı adresin iki
// yazımı iki kanal olmasın (1.1'den beri aynı kural).
func TestNormalizeLowercasesEmailButNotWebhook(t *testing.T) {
	t.Parallel()

	email := Normalize(ChannelInput{
		Kind:   store.NotificationChannelKindEmail,
		Name:   "  Nöbetçi  ",
		Target: "  Nobetci@Example.COM ",
	})

	if email.Target != "nobetci@example.com" {
		t.Errorf("e-posta hedefi = %q", email.Target)
	}
	if email.Name != "Nöbetçi" {
		t.Errorf("ad = %q", email.Name)
	}

	// Webhook yolu BÜYÜK/küçük harfe duyarlı: `/Hook` ile `/hook` farklı
	// kaynaklar olabilir.
	webhook := Normalize(ChannelInput{
		Kind:   store.NotificationChannelKindWebhook,
		Name:   "Köprü",
		Target: " https://Ornek.example/Hook ",
	})

	if webhook.Target != "https://Ornek.example/Hook" {
		t.Errorf("webhook hedefi = %q", webhook.Target)
	}
}

// Kayıtta olmayan bir tür için gönderici istemek sessizce başarısız olmamalı.
func TestRegistryReportsUnsupportedKind(t *testing.T) {
	t.Parallel()

	registry := NewRegistryWith(NewMockEmailChannel(nil, discardLogger()))

	if _, err := registry.For(store.NotificationChannelKindEmail); err != nil {
		t.Fatalf("kayıtlı tür reddedildi: %v", err)
	}

	if _, err := registry.For(store.NotificationChannelKindWebhook); !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("hata = %v, beklenen ErrUnsupportedKind", err)
	}
}

// Varsayılan kayıt İKİ türü de karşılıyor: şemadaki enum ile adapter listesi
// ayrışırsa o kanala yapılan her gönderim sonsuza kadar başarısız olurdu.
func TestDefaultRegistryCoversEveryKind(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(safedial.Policy{AllowPrivate: true}, nil, discardLogger())

	for _, kind := range []store.NotificationChannelKind{
		store.NotificationChannelKindEmail,
		store.NotificationChannelKindWebhook,
	} {
		if _, err := registry.For(kind); err != nil {
			t.Errorf("%q için gönderici yok: %v", kind, err)
		}
	}
}

// 7.4'ün kararı: WEBHOOK GERÇEKTEN GÖNDERİYOR, e-posta mock kalıyor.
//
// Ayrım kod içinde tek bir yerde (NewRegistry) ve gözle görülmüyor: yerine
// istek atmayan bir adapter konsa gönderim defteri "sent" yazmaya devam
// eder, hiçbir test kırılmaz ve kimse fark etmez. Bu test o sessiz geri
// dönüşü kapatıyor.
func TestDefaultRegistryUsesRealWebhookSender(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(safedial.Policy{AllowPrivate: true}, nil, discardLogger())

	webhook, err := registry.For(store.NotificationChannelKindWebhook)
	if err != nil {
		t.Fatalf("webhook göndericisi yok: %v", err)
	}

	if _, ok := webhook.(*HTTPWebhookChannel); !ok {
		t.Errorf("webhook adapter'ı %T, beklenen *HTTPWebhookChannel (7.4: gerçek gönderim)", webhook)
	}
}

// ÜRETİMDE MOCK ADAPTER SESSİZ KALMAMALI (7.5).
//
// Mock e-posta adapter'ı gönderimi yalnızca loga yazıyor; bu geliştirmede
// TESLİMAT YOLUNUN KENDİSİ (6.4'ün akışı klonlayan biri için ancak böyle
// denenebiliyor). Ama üretimde çalışırsa kimse e-posta almıyor ve gönderim
// defteri `sent` yazmaya devam ettiği için kimse fark etmiyor.
func TestRegistryWarnsAboutMockAdaptersInProduction(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(safedial.Policy{}, nil, discardLogger())

	warnings := registry.MockWarnings(true)
	if len(warnings) != 1 {
		t.Fatalf("uyarı sayısı = %d, beklenen 1 (%v)", len(warnings), warnings)
	}

	if !strings.Contains(warnings[0], string(store.NotificationChannelKindEmail)) {
		t.Errorf("uyarı e-posta kanalını göstermiyor: %q", warnings[0])
	}

	// Webhook 7.4'ten beri GERÇEKTEN gönderiyor: uyarıya girmemeli.
	if strings.Contains(warnings[0], string(store.NotificationChannelKindWebhook)) {
		t.Errorf("gerçek gönderici mock diye uyarıldı: %q", warnings[0])
	}
}

// Geliştirme ve testte uyarı YOK: orada mock zaten beklenen davranış ve her
// açılışta tekrarlanan bir uyarı, gerçek uyarıları gürültüde boğardı.
func TestRegistryStaysQuietOutsideProduction(t *testing.T) {
	t.Parallel()

	registry := NewRegistry(safedial.Policy{}, nil, discardLogger())

	if warnings := registry.MockWarnings(false); len(warnings) != 0 {
		t.Errorf("üretim dışında uyarı üretildi: %v", warnings)
	}
}

// Gerçek adapter'larla kurulmuş bir kayıt üretimde de sessiz: uyarı "mock
// kullanılıyor mu" sorusuna cevap veriyor, "üretimdeyiz" diye bağırmıyor.
func TestRegistryStaysQuietWhenEveryAdapterIsReal(t *testing.T) {
	t.Parallel()

	registry := NewRegistryWith(NewHTTPWebhookChannel(safedial.Policy{}, discardLogger()))

	if warnings := registry.MockWarnings(true); len(warnings) != 0 {
		t.Errorf("gerçek adapter için uyarı üretildi: %v", warnings)
	}
}
