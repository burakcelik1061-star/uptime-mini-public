// Package realtime, canlı yayının taşıyıcısıdır: olayları worker sürecinden
// api sürecine taşır (Postgres LISTEN/NOTIFY) ve api içindeki açık WebSocket
// bağlantılarına dağıtır (Hub).
//
// Sorumluluk sınırları (bu projede paketler bilinçli olarak dar):
//
//	internal/realtime → OLAYIN TAŞINMASI ve DAĞITILMASI
//	internal/httpapi  → WebSocket el sıkışması, kimlik doğrulama, bağlantı ömrü
//	internal/scheduler + internal/incidents → olayın DOĞDUĞU yer (6.2)
//
// Bu paket bir olayın ne anlama geldiğini BİLMİYOR: `Event.Type` bir dize,
// `Event.Data` ham JSON. Böylece yeni bir olay türü eklemek taşıma katmanına
// hiç dokunmuyor.
package realtime

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// NotifyChannel, Postgres NOTIFY kanalının adı.
//
// TEK BİR POSTGRES KANALI VAR, org başına bir tane değil: `LISTEN` bir
// tanımlayıcı alıyor, yani org başına kanal demek yeni bir org doğduğunda
// api'nin dinleme listesini büyütmesi (ve bunu kaçırdığında o org'un sessiz
// kalması) demekti. Yönlendirme bu yüzden PAYLOAD'ın içinde: api tek kanalı
// dinliyor, olayı doğru abonelere Hub dağıtıyor.
const NotifyChannel = "uptime_realtime"

// maxNotifyPayload, Postgres'in NOTIFY payload sınırı (8000 bayt).
//
// Aşılırsa veritabanı ifadeyi reddeder; sınır burada da duruyor ki hata
// "sorgu patladı" olarak değil "olay çok büyük" olarak görünsün. Sözleşme
// gereği olaylar İŞARETÇİ niteliğinde küçük kalmalı (kimlik + durum);
// istemcinin ayrıntıya ihtiyacı varsa HTTP ucundan okur.
const maxNotifyPayload = 8000

// Event, canlı yayına giren tek bir olay.
type Event struct {
	// Type, olayın makine tarafı (ör. "pong", "monitor.status_changed").
	Type string `json:"type"`
	// At, olayın SUNUCUDA oluştuğu an. İstemci "bu bilgi ne kadar taze"
	// sorusunu kendi saatine bakmadan cevaplayabilsin.
	At time.Time `json:"at"`
	// Data, olaya özgü gövde. Ham JSON: taşıma katmanı içeriği yorumlamıyor.
	Data json.RawMessage `json:"data,omitempty"`
}

// NewEvent, verilen gövdeyi JSON'a çevirerek bir olay üretir.
func NewEvent(eventType string, at time.Time, data any) (Event, error) {
	event := Event{Type: eventType, At: at}

	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return Event{}, fmt.Errorf("olay gövdesi kodlanamadı: %w", err)
		}

		event.Data = encoded
	}

	return event, nil
}

// envelope, Postgres NOTIFY payload'ının biçimi: hedef kanal + olay.
type envelope struct {
	Channel string `json:"channel"`
	Event   Event  `json:"event"`
}

// OrgChannel, bir organizasyonun OTURUMLU kanalının adı.
//
// Kanal adı organizasyon kimliğini içeriyor ve abone olurken kullanılan
// kimlik, el sıkışmadaki biletin İÇİNDEN geliyor (bkz. auth.TicketIdentity) —
// yani istemcinin gönderdiği bir değerden değil, sunucunun doğruladığı bir
// değerden. Bir org'un olayının başka bir org'a sızması için kanal adının
// yanlış üretilmesi gerekir, istemcinin yanlış bir şey söylemesi değil.
func OrgChannel(orgID uuid.UUID) string {
	return "org:" + orgID.String()
}
