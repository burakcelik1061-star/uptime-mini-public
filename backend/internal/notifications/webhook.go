package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// HTTPWebhookChannel, webhook'u GERÇEKTEN çağıran adapter (7.4).
//
// 6.3 bu yeri işaretleyerek bırakmıştı: "gerçek göndericiyi eklemek, SSRF
// korumasını da birlikte eklemek demek". Sebebi bu adapter'ın tanımında:
// hedef adresi KULLANICI yazıyor ve sunucu ona istek atıyor. Koruma
// `net.Dialer.Control` katmanında (bkz. internal/safedial), yani
// yönlendirmeler ve DNS rebinding de kapsanıyor.
//
// E-posta tarafı MOCK KALIYOR (GÜNCEL DURUM §2: hesap gerektiren dış servis
// yok). Webhook'un gerçekleşebilmesinin sebebi, karşı tarafın kullanıcının
// KENDİ sunucusu olması — hiçbir sağlayıcı hesabı gerekmiyor.
type HTTPWebhookChannel struct {
	client *http.Client
	logger *slog.Logger
}

const (
	// webhookTimeout, tek bir gönderimin üst sınırı.
	//
	// Kısa: gönderim worker'ın job döngüsünde koşuyor ve yavaş bir alıcı
	// diğer bildirimleri bekletmemeli. Yeniden deneme zaten var (6.3: geri
	// çekilmeli ve 3 denemeyle sınırlı).
	webhookTimeout = 10 * time.Second

	// webhookMaxResponseBytes, alıcının cevabından okunacak üst sınır.
	//
	// Cevabın İÇERİĞİ kullanılmıyor (başarı ölçütü durum kodu), ama gövde
	// okunmadan bağlantı kapatılırsa alıcı tarafta "broken pipe" görünür.
	webhookMaxResponseBytes = 8 << 10
)

// NewHTTPWebhookChannel, gerçek göndericiyi kurar.
//
// `policy` ZORUNLU argüman: varsayılanı olsaydı korumayı unutmak mümkün
// olurdu (checker.New ile aynı gerekçe).
func NewHTTPWebhookChannel(policy safedial.Policy, logger *slog.Logger) *HTTPWebhookChannel {
	if logger == nil {
		logger = slog.Default()
	}

	return &HTTPWebhookChannel{
		client: &http.Client{
			Timeout: webhookTimeout,
			Transport: &http.Transport{
				// SSRF KORUMASI (7.4).
				DialContext: policy.Dialer(5 * time.Second).DialContext,
				// Bağlantı yeniden kullanılmıyor: farklı organizasyonların
				// webhook'ları aynı bağlantıyı paylaşmasın.
				DisableKeepAlives:   true,
				TLSHandshakeTimeout: 5 * time.Second,
				// Proxy ortam değişkenleri okunmuyor: aradaki bir proxy
				// korumanın gördüğü adresi değiştirirdi (`Control` proxy'nin
				// adresini denetlerdi, hedefin değil).
				Proxy: nil,
			},
			// YÖNLENDİRME HİÇ İZLENMİYOR: bir webhook alıcısının
			// yönlendirmeye ihtiyacı yok ve her yönlendirme, kullanıcının
			// yazdığı adresin başka bir yere gitmesi demek. SSRF koruması
			// her durağı zaten denetliyor; izlemeyerek yüzeyi büsbütün
			// kapatıyoruz (3xx aşağıda başarısız sayılıyor).
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// Kind, adapter'ın karşıladığı tür.
func (c *HTTPWebhookChannel) Kind() store.NotificationChannelKind {
	return store.NotificationChannelKindWebhook
}

// webhookPayload, alıcıya giden gövde.
//
// İÇ ALAN ADLARI DEĞİL, SÖZLEŞME: bu gövde dışarıya verilen bir arayüz.
// `store` tiplerini doğrudan serileştirmek, bir sütun adı değiştiğinde
// kullanıcıların entegrasyonunu kırardı (frontend'in api-types.ts ile
// backend şeması arasındaki ayrımın aynısı, 0.2).
type webhookPayload struct {
	Event     string    `json:"event"`
	Org       string    `json:"organization"`
	Incident  string    `json:"incident"`
	Impact    string    `json:"impact"`
	StartedAt time.Time `json:"startedAt"`
	Body      string    `json:"body,omitempty"`
}

// Send, webhook'u çağırır.
func (c *HTTPWebhookChannel) Send(ctx context.Context, target string, msg Message) error {
	payload, err := json.Marshal(webhookPayload{
		Event:     string(msg.Event),
		Org:       msg.OrgName,
		Incident:  msg.Title,
		Impact:    string(msg.Impact),
		StartedAt: msg.StartedAt,
		Body:      msg.Body,
	})
	if err != nil {
		return fmt.Errorf("webhook gövdesi hazırlanamadı: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook isteği kurulamadı: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", webhookUserAgent)

	response, err := c.client.Do(request)
	if err != nil {
		// SSRF koruması reddettiyse bunu AYRI söylüyoruz: kullanıcı
		// adresinin neden hiç denenmediğini anlayabilmeli ve bu bir ağ
		// arızası değil, bizim kararımız. (Kanal defterindeki `last_error`
		// alanına giden metin — iç ağ topolojisini anlatmıyor.)
		if errors.Is(err, safedial.ErrBlocked) {
			return fmt.Errorf("%w", ErrTargetBlocked)
		}

		return fmt.Errorf("webhook çağrılamadı: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	// Gövde okunuyor ama tutulmuyor.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, webhookMaxResponseBytes))

	// BAŞARI ÖLÇÜTÜ 2xx. 3xx de başarısız sayılıyor: yönlendirme
	// izlenmiyor (yukarıya bkz.), yani 302 dönen bir alıcı bildirimi
	// almamış demektir.
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("webhook %d döndü", response.StatusCode)
	}

	c.logger.Info("webhook bildirimi gönderildi",
		slog.String("url", target),
		slog.String("event", string(msg.Event)),
		slog.Int("status", response.StatusCode))

	return nil
}

// webhookUserAgent, alıcının loglarında kim olduğumuz görünsün.
const webhookUserAgent = "uptime-mini/1.0 (+https://github.com/burakcelik1061-star/uptime-mini)"
