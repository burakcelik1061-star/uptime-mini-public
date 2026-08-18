package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// GELİŞTİRİCİ POSTA KUTUSU (8.2) — gerçek veritabanına karşı.
//
// Testlerin konusu tek bir cümle: "mock e-posta adapter'ı gönderdiğini
// SAKLIYOR mu, ve saklayamadığında gönderimi düşürüyor mu". İkincisi asıl
// olan — posta kutusu bir geliştirme aracı ve arızası 6.4'ün abonelik akışını
// bozmamalı.

// failingOutboxStore, yazma sırasında patlayan bir posta kutusu deposu.
type failingOutboxStore struct{ err error }

func (s *failingOutboxStore) RecordOutboxMessage(context.Context, store.RecordOutboxMessageParams) error {
	return s.err
}

func (s *failingOutboxStore) ListOutboxMessages(context.Context, int32) ([]store.EmailOutbox, error) {
	return nil, s.err
}

// VERİTABANINA DOKUNAN TESTLER `t.Parallel()` KULLANMIYOR (paketin mevcut
// kuralı): `testdb.Pool` her çağrıda şemayı TRUNCATE ediyor, yani paralel iki
// test birbirinin satırlarını siler. 1.3 aynı dersi paketler arasında almıştı;
// burada aynısı paket İÇİNDE geçerli.
func TestMockEmailChannelRecordsToOutbox(t *testing.T) {
	pool := testdb.Pool(t, "notifications")
	queries := store.New(pool)
	outbox := NewOutbox(queries)

	channel := NewMockEmailChannel(outbox, discardLogger())

	msg := Message{
		Event:   store.NotificationEventIncidentOpened,
		OrgName: "Acme",
		Title:   "Aboneliğinizi onaylayın",
		Body:    "Onaylamak için: http://localhost:3100/status/acme/confirm?token=abc",
	}

	if err := channel.Send(t.Context(), "ziyaretci@example.com", msg); err != nil {
		t.Fatalf("gönderim başarısız: %v", err)
	}

	messages, err := outbox.List(t.Context())
	if err != nil {
		t.Fatalf("posta kutusu okunamadı: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("mesaj sayısı = %d, beklenen 1", len(messages))
	}

	got := messages[0]
	if got.Recipient != "ziyaretci@example.com" {
		t.Errorf("alıcı = %q", got.Recipient)
	}

	// KONU SATIRI ADAPTER'IN ÜRETTİĞİ METİN (6.3): posta kutusu gerçekten
	// gönderilmiş olanı saklıyor, sonradan yeniden türetilmiş bir şeyi değil.
	if want := "[Acme] Kesinti: Aboneliğinizi onaylayın"; got.Subject != want {
		t.Errorf("konu = %q, beklenen %q", got.Subject, want)
	}

	// GÖVDEDEKİ LİNK AYNEN DURUYOR: 8.2'nin bütün amacı bu linke terminal
	// açmadan tıklanabilmesi.
	if got.Body != msg.Body {
		t.Errorf("gövde = %q", got.Body)
	}
}

// Posta kutusu KAPALIYKEN (nil) adapter 6.3'teki davranışını birebir
// sürdürmeli: yalnızca loglar, hata dönmez, hiçbir şey saklanmaz.
func TestMockEmailChannelWithoutOutboxStillSends(t *testing.T) {
	t.Parallel()

	channel := NewMockEmailChannel(nil, discardLogger())

	if err := channel.Send(t.Context(), "kimse@example.com", Message{
		Event:   store.NotificationEventIncidentResolved,
		OrgName: "Acme",
		Title:   "Çözüldü",
	}); err != nil {
		t.Fatalf("posta kutusu kapalıyken gönderim başarısız: %v", err)
	}
}

// POSTA KUTUSU YAZILAMADIĞINDA GÖNDERİM DÜŞMEMELİ.
//
// Bu testin konusu bir tercih: hata döndürmek, geliştirme aracının arızasını
// ürünün arızasına çevirirdi (6.4'ün abonelik ucu 500 döner, 8.3'ün daveti
// yazılmazdı). Gönderimin kaydı log satırında duruyor.
func TestMockEmailChannelSurvivesOutboxFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("posta kutusu düştü")
	channel := NewMockEmailChannel(NewOutbox(&failingOutboxStore{err: boom}), discardLogger())

	if err := channel.Send(t.Context(), "birisi@example.com", Message{
		Event:   store.NotificationEventIncidentOpened,
		OrgName: "Acme",
		Title:   "Kesinti",
	}); err != nil {
		t.Fatalf("posta kutusu hatası gönderimi düşürdü: %v", err)
	}
}

// Bağlanmamış bir posta kutusunu OKUMAK sessizce boş liste dönmemeli:
// çalışmayan bir yapılandırma, "hiç e-posta gönderilmedi" gibi okunurdu.
func TestOutboxListWithoutStoreFails(t *testing.T) {
	t.Parallel()

	var outbox *Outbox

	if _, err := outbox.List(t.Context()); err == nil {
		t.Fatal("bağlanmamış posta kutusu hata dönmedi")
	}
}

// Liste EN YENİDEN ESKİYE geliyor: ekranda en son gönderilen link üstte
// olmalı (kullanıcının aradığı şey neredeyse her zaman o).
func TestOutboxListReturnsNewestFirst(t *testing.T) {
	pool := testdb.Pool(t, "notifications")
	queries := store.New(pool)
	outbox := NewOutbox(queries)
	channel := NewMockEmailChannel(outbox, discardLogger())

	for _, title := range []string{"birinci", "ikinci", "üçüncü"} {
		if err := channel.Send(t.Context(), title+"@example.com", Message{
			Event:   store.NotificationEventIncidentOpened,
			OrgName: "Acme",
			Title:   title,
		}); err != nil {
			t.Fatalf("gönderim başarısız: %v", err)
		}
	}

	messages, err := outbox.List(t.Context())
	if err != nil {
		t.Fatalf("posta kutusu okunamadı: %v", err)
	}

	if len(messages) != 3 {
		t.Fatalf("mesaj sayısı = %d, beklenen 3", len(messages))
	}

	if messages[0].Recipient != "üçüncü@example.com" {
		t.Errorf("ilk sıradaki = %q, beklenen en yeni gönderim", messages[0].Recipient)
	}
}

// KONU SATIRI TÜRE GÖRE DEĞİŞİYOR (8.3'ün düzeltmesi).
//
// 6.4 ve 8.3'ün işlem e-postaları `Message`ı kesinti bildirimi olarak
// taşıyordu ve konu "[Acme] KESİNTİ: Aboneliğinizi onaylayın" oluyordu — bir
// onay e-postası kesinti gibi görünüyordu. Hata 8.2'nin posta kutusu ekranı
// gelene kadar GÖRÜNMEDİ, çünkü gönderim yalnızca loga yazılıyordu.
func TestSubjectDistinguishesTransactionalFromIncident(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		msg  Message
		want string
	}{
		"kesinti açılışı": {
			msg:  Message{OrgName: "Acme", Title: "API yanıt vermiyor"},
			want: "[Acme] Kesinti: API yanıt vermiyor",
		},
		"kesinti kapanışı": {
			msg: Message{
				Event:   store.NotificationEventIncidentResolved,
				OrgName: "Acme",
				Title:   "API yanıt vermiyor",
			},
			want: "[Acme] Çözüldü: API yanıt vermiyor",
		},
		"işlem e-postası": {
			msg: Message{
				Kind:    MessageKindTransactional,
				OrgName: "Acme",
				Title:   "Organizasyona davet edildiniz",
			},
			want: "[Acme] Organizasyona davet edildiniz",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := subjectFor(tc.msg); got != tc.want {
				t.Errorf("konu = %q, beklenen %q", got, tc.want)
			}
		})
	}
}
