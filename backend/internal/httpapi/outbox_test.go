package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// GELİŞTİRİCİ POSTA KUTUSU UCU (8.2).
//
// BU DOSYADAKİ EN ÖNEMLİ TEST İLKİ: kapı kapalıyken ucun VAR OLMAMASI.
// Koruma bir handler kontrolü değil, ucun router'a hiç kaydedilmemesi — ve
// "kaydedilmedi" ancak dışarıdan istek atarak doğrulanabilir.

// fakeOutboxStore, posta kutusunun sahte deposu.
type fakeOutboxStore struct {
	rows []store.EmailOutbox
}

func (s *fakeOutboxStore) RecordOutboxMessage(
	_ context.Context, arg store.RecordOutboxMessageParams,
) error {
	s.rows = append(s.rows, store.EmailOutbox{
		ID:        uuid.New(),
		Recipient: arg.Recipient,
		Subject:   arg.Subject,
		Body:      arg.Body,
		CreatedAt: time.Now(),
	})

	return nil
}

func (s *fakeOutboxStore) ListOutboxMessages(_ context.Context, _ int32) ([]store.EmailOutbox, error) {
	return s.rows, nil
}

// KAPI KAPALIYKEN UÇ HİÇ YOK: geçerli bir token'la bile 404.
//
// 404, 403 DEĞİL: uç üretimde gerçekten mevcut değil ve "yetkin yetmiyor"
// demek, orada bir şey olduğunu söylemek olurdu (1.4'ün "org yok ile üye
// değilsin aynı cevabı alır" disiplininin aynısı).
func TestOutboxEndpointIsAbsentWhenDevToolsDisabled(t *testing.T) {
	// newTestDeps `Outbox` alanını doldurmuyor — yani varsayılan router,
	// üretimdeki router'ın ta kendisi.
	deps := newTestDeps(t)
	if deps.Outbox != nil {
		t.Fatal("varsayılan test bağımlılıkları posta kutusunu açıyor; kapı testi anlamsız olurdu")
	}

	router := NewRouter(deps)
	token := authenticate(t, router, "posta-kutusu@example.com")

	rec := do(t, router, http.MethodGet, "/v1/dev/outbox", "", bearer(token))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("kapalı kapıda durum = %d, beklenen 404 (gövde: %s)", rec.Code, rec.Body.String())
	}
}

// Kapı AÇIKKEN uç saklanan mesajları döndürüyor.
func TestOutboxEndpointListsRecordedMessages(t *testing.T) {
	fake := &fakeOutboxStore{}
	outbox := notifications.NewOutbox(fake)

	// Mesaj, ADAPTER üzerinden yazılıyor: testin ölçtüğü şey "tabloya satır
	// koyduk mu" değil, GÖNDERİM YOLUNUN posta kutusuna çıkması.
	channel := notifications.NewMockEmailChannel(
		outbox, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := channel.Send(t.Context(), "davetli@example.com", notifications.Message{
		Event:   store.NotificationEventIncidentOpened,
		OrgName: "Acme",
		Title:   "Davet",
		Body:    "Katılmak için: http://localhost:3100/invitations/accept?token=abc",
	}); err != nil {
		t.Fatalf("gönderim başarısız: %v", err)
	}

	deps := newTestDeps(t)
	deps.Outbox = outbox

	router := NewRouter(deps)
	token := authenticate(t, router, "posta-kutusu-acik@example.com")

	rec := do(t, router, http.MethodGet, "/v1/dev/outbox", "", bearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, beklenen 200 (gövde: %s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Messages []struct {
			Recipient string `json:"recipient"`
			Subject   string `json:"subject"`
			Body      string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("cevap çözümlenemedi: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("mesaj sayısı = %d, beklenen 1", len(body.Messages))
	}

	if body.Messages[0].Recipient != "davetli@example.com" {
		t.Errorf("alıcı = %q", body.Messages[0].Recipient)
	}

	// Gövde AYNEN dönüyor: ekran linki ancak böyle tıklanabilir yapabiliyor.
	if body.Messages[0].Body == "" {
		t.Error("gövde boş döndü; posta kutusunun tek işi linki taşımak")
	}
}

// Kapı açık olsa bile uç OTURUM İSTİYOR: pano ekranı, oturumsuz bir yüzey değil.
func TestOutboxEndpointRequiresAuthentication(t *testing.T) {
	deps := newTestDeps(t)
	deps.Outbox = notifications.NewOutbox(&fakeOutboxStore{})

	router := NewRouter(deps)

	rec := do(t, router, http.MethodGet, "/v1/dev/outbox", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokensiz durum = %d, beklenen 401", rec.Code)
	}
}
