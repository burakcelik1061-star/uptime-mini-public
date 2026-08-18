package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/subscribers"
)

// Status page abonelikleri (Faz 6.4) — 5.1'in OTURUMSUZ public ağacında.
//
// ZİYARETÇİ İLK KEZ YAZIYOR: 0.1 §2'nin "ziyaretçi hiçbir şey yazmaz"
// kuralının bilinçli istisnası. Yazdığı tek şey kendi e-posta adresi.
//
// KİMLİK YOK, TOKEN VAR: abonelik bir `User` değil (0.1). Onay ve iptal
// linklerindeki tahmin edilemez token'lar tek kimlik aracı.

// SubscriberService, abonelik uçlarının ihtiyaç duyduğu davranışlar.
type SubscriberService interface {
	Subscribe(ctx context.Context, slug, email string) error
	Confirm(ctx context.Context, token string) error
	Unsubscribe(ctx context.Context, token string) (string, error)
}

type subscriberHandler struct {
	subscribers SubscriberService
	logger      *slog.Logger
}

type subscribeRequest struct {
	Email string `json:"email"`
}

// subscribeResponse, HER DURUMDA aynı gövde.
type subscribeResponse struct {
	Message string `json:"message"`
}

// tokenRequest, onay ve iptal uçlarının gövdesi.
type tokenRequest struct {
	Token string `json:"token"`
}

// subscribe — POST /v1/public/status/{slug}/subscribers
//
// ENUMERATION'A KAPALI ve bu ucun en önemli özelliği bu.
//
// "Bu adres zaten kayıtlı" gibi bir cevap, o sayfaya kimin abone olduğunu
// dışarıya söylerdi: bir yabancı adres listesini tek tek deneyerek çıkarım
// yapabilirdi. Bu yüzden ÜÇ DURUM DA AYNI CEVABI alıyor — yeni abone ·
// bekleyen abone · zaten onaylı abone. Bilinmeyen/yayımlanmamış SAYFA da aynı
// cevabı alıyor (5.1'in "yayımlanmamış ≡ olmayan" kuralının devamı).
//
// Ayırt edilen TEK durum, adresin BİÇİM hatası: o bilgi kayıt durumu hakkında
// bir şey söylemiyor, yalnızca yazım hatasını gösteriyor.
//
// 202 (Accepted), 201 DEĞİL: sunucu "isteğini aldım" diyor, "kaydettim"
// demiyor — kaydın gerçekleşip gerçekleşmediği zaten söylenmeyecek bilgi.
//
// Hız sınırlama 7.4'ün konusu: bu uç, e-posta gönderimini tetikleyen tek
// oturumsuz yüzey.
func (h *subscriberHandler) subscribe(w http.ResponseWriter, r *http.Request) {
	var req subscribeRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	err := h.subscribers.Subscribe(r.Context(), chi.URLParam(r, "slug"), req.Email)

	switch {
	case err == nil, errors.Is(err, subscribers.ErrNotFound):
		// Bilinmeyen sayfa da BAŞARILI gibi cevaplanıyor.
		writeJSON(w, http.StatusAccepted, subscribeResponse{
			Message: "Adresinize bir onay bağlantısı gönderildi.",
		})

	case errors.Is(err, subscribers.ErrInvalidEmail):
		(&apiError{
			status:  http.StatusUnprocessableEntity,
			code:    codeValidationFailed,
			message: "gövde doğrulanamadı",
			fields:  []FieldError{{Field: "email", Message: "geçerli bir e-posta adresi olmalı"}},
		}).write(w, r)

	default:
		writeInternalError(w, r, h.logger, "abonelik oluşturulamadı", err)
	}
}

// confirm — POST /v1/public/subscriptions/confirm
//
// Çift onayın ikinci adımı. Token TEK KULLANIMLIK (sorguda yakılıyor), yani
// aynı link ikinci kez çalışmıyor.
func (h *subscriberHandler) confirm(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	if err := h.subscribers.Confirm(r.Context(), req.Token); err != nil {
		if errors.Is(err, subscribers.ErrNotFound) {
			// Tanınmayan ve kullanılmış token AYNI cevabı alıyor.
			writeError(w, r, http.StatusNotFound, codeNotFound,
				"bağlantı geçersiz veya daha önce kullanılmış")

			return
		}

		writeInternalError(w, r, h.logger, "abonelik onaylanamadı", err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// unsubscribe — POST /v1/public/subscriptions/unsubscribe
func (h *subscriberHandler) unsubscribe(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	if _, err := h.subscribers.Unsubscribe(r.Context(), req.Token); err != nil {
		if errors.Is(err, subscribers.ErrNotFound) {
			// Zaten iptal edilmiş bir abonelik de buraya düşüyor (satır
			// silindiği için token artık eşleşmiyor) ve aynı cevabı alıyor.
			writeError(w, r, http.StatusNotFound, codeNotFound,
				"bağlantı geçersiz veya abonelik zaten iptal edilmiş")

			return
		}

		writeInternalError(w, r, h.logger, "abonelik iptal edilemedi", err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}
