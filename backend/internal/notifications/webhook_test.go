package notifications

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

func testMessage() Message {
	return Message{
		Event:     store.NotificationEventIncidentOpened,
		Title:     "Ödeme Servisi yanıt vermiyor",
		Impact:    store.IncidentImpactMajor,
		OrgName:   "Acme Corp",
		StartedAt: time.Now().UTC(),
		Body:      "İnceleniyor",
	}
}

// İÇ AĞ HEDEFİ REDDEDİLİYOR ve hata AYIRT EDİLEBİLİR (7.4): 6.3'te
// işaretlenmiş olan "gerçek göndericiyi eklemek korumayı da eklemek demek"
// notunun karşılığı.
func TestWebhookRefusesInternalTarget(t *testing.T) {
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	channel := NewHTTPWebhookChannel(safedial.Policy{}, discardLogger())

	err := channel.Send(t.Context(), server.URL, testMessage())
	if err == nil {
		t.Fatal("loopback hedefe gönderim başarılı oldu")
	}
	if !errors.Is(err, ErrTargetBlocked) {
		t.Errorf("hata = %v, beklenen ErrTargetBlocked (aksi hâlde çağıran 500 döner)", err)
	}
	if reached {
		t.Error("istek hedefe ULAŞTI")
	}
}

// Kapı açıkken gönderim gerçekten yapılıyor: gövde ve başlıklar sözleşmede.
func TestWebhookPostsPayload(t *testing.T) {
	type received struct {
		method      string
		contentType string
		userAgent   string
		body        string
	}

	var got received
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 2048)
		n, _ := r.Body.Read(buf)

		got = received{
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			userAgent:   r.Header.Get("User-Agent"),
			body:        string(buf[:n]),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	channel := NewHTTPWebhookChannel(safedial.Policy{AllowPrivate: true}, discardLogger())

	if err := channel.Send(t.Context(), server.URL, testMessage()); err != nil {
		t.Fatalf("gönderim başarısız: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("metot = %q, beklenen POST", got.method)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q", got.contentType)
	}
	if got.userAgent == "" {
		t.Error("User-Agent boş — alıcının loglarında kim olduğumuz görünmeli")
	}
	for _, want := range []string{"incident_opened", "Acme Corp", "Ödeme Servisi"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("gövdede %q yok: %s", want, got.body)
		}
	}
}

// 2xx DIŞI cevap BAŞARISIZ: 6.3'ün yeniden deneme mekanizması ancak hata
// dönerse çalışıyor.
func TestWebhookTreatsNon2xxAsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	channel := NewHTTPWebhookChannel(safedial.Policy{AllowPrivate: true}, discardLogger())

	if err := channel.Send(t.Context(), server.URL, testMessage()); err == nil {
		t.Fatal("500 dönen alıcı başarılı sayıldı")
	}
}

// YÖNLENDİRME İZLENMİYOR: 3xx de başarısız.
func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var finalReached bool
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		finalReached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	channel := NewHTTPWebhookChannel(safedial.Policy{AllowPrivate: true}, discardLogger())

	if err := channel.Send(t.Context(), redirector.URL, testMessage()); err == nil {
		t.Error("yönlendirme başarılı sayıldı")
	}
	if finalReached {
		t.Error("yönlendirme İZLENDİ — kullanıcının yazdığı adres başka bir yere gitti")
	}
}
