package notifications

// GELİŞTİRİCİ POSTA KUTUSU (Faz 8.2).
//
// NEDEN BU PAKETTE ve ayrı bir paket değil: outbox, MOCK E-POSTA ADAPTER'ININ
// posta kutusu — gerçek bir sağlayıcıya geçildiğinde (NewRegistry'de tek satır)
// kendiliğinden ortadan kalkması gereken bir GELİŞTİRME ARACI. Ayrı bir pakete
// koymak, 6.3'ün "e-posta soyutlaması TEK yerde" kuralının yanına ikinci bir
// katman koymak olurdu; bu adımın açık şartı da buydu.
//
// Bu dosya iki YÖNÜ de taşıyor: yazma (adapter kullanıyor) ve okuma (panodaki
// ekran kullanıyor). İkisini ayırmak, aynı üç sütun için iki dosya demekti.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// maxOutboxMessages, ekranın gösterdiği en fazla mesaj sayısı.
//
// Sınır SUNUCUDA: 00018 bilinçli olarak budama job'ı taşımıyor, yani tablo
// geliştirme ortamında yavaşça büyüyor. Sınırı istemciye bırakmak, altı ay
// açık duran bir ortamda tüm tabloyu ağdan geçirmek olurdu.
const maxOutboxMessages = 100

// OutboxStore, posta kutusunun ihtiyaç duyduğu sorgular.
type OutboxStore interface {
	RecordOutboxMessage(ctx context.Context, arg store.RecordOutboxMessageParams) error
	ListOutboxMessages(ctx context.Context, rowLimit int32) ([]store.EmailOutbox, error)
}

// Outbox, mock e-posta adapter'ının sakladığı gönderimler.
//
// nil bir *Outbox GEÇERLİ ve "posta kutusu kapalı" demek: üretimde (ve outbox
// bağlanmamış her kurulumda) adapter yalnızca loga yazmaya devam ediyor.
// Bayrak yerine nil kullanılıyor çünkü kapalı olma hâli zaten "bağımlılık
// verilmedi" ile aynı şey.
type Outbox struct {
	queries OutboxStore
}

// NewOutbox, posta kutusunu kurar.
func NewOutbox(queries OutboxStore) *Outbox {
	return &Outbox{queries: queries}
}

// OutboxMessage, saklanmış tek bir gönderim.
type OutboxMessage struct {
	ID        uuid.UUID
	Recipient string
	Subject   string
	Body      string
	CreatedAt time.Time
}

// record, gönderimi saklar.
func (o *Outbox) record(ctx context.Context, recipient string, msg Message) error {
	if o == nil {
		return nil
	}

	return o.queries.RecordOutboxMessage(ctx, store.RecordOutboxMessageParams{
		Recipient: recipient,
		Subject:   subjectFor(msg),
		Body:      msg.Body,
	})
}

// List, en yeni mesajları döner.
func (o *Outbox) List(ctx context.Context) ([]OutboxMessage, error) {
	if o == nil {
		// Buraya düşmek, ucun outbox bağlanmadan kaydedildiği anlamına gelir.
		// Boş liste dönmek yerine hata: sessiz bir "hiç mesaj yok" ekranı,
		// çalışmayan bir yapılandırmayı çalışıyor gibi gösterirdi.
		return nil, fmt.Errorf("posta kutusu bağlı değil")
	}

	rows, err := o.queries.ListOutboxMessages(ctx, maxOutboxMessages)
	if err != nil {
		return nil, fmt.Errorf("posta kutusu okunamadı: %w", err)
	}

	messages := make([]OutboxMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, OutboxMessage{
			ID:        row.ID,
			Recipient: row.Recipient,
			Subject:   row.Subject,
			Body:      row.Body,
			CreatedAt: row.CreatedAt,
		})
	}

	return messages, nil
}
