package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/passwordreset"
)

// PAROLA SIFIRLAMA (Faz 8.4) — OTURUMSUZ yüzey.
//
// İki uç da `/v1/auth/*` ağacında, yani 7.4'ün hız sınırını (10/dk) taşıyor.
// Sınır burada özellikle anlamlı: istek ucu e-posta gönderimini tetikleyen
// ikinci oturumsuz uç (birincisi 6.4'ün aboneliği) ve onay ucu tahmin
// edilemez bir token doğruluyor.

// PasswordResetService, sıfırlama uçlarının ihtiyaç duyduğu davranışlar.
type PasswordResetService interface {
	// Request HATA DÖNDÜRMÜYOR ve bu imzanın kendisi bir karar: adresin
	// kayıtlı olup olmadığı çağırana hiçbir kanaldan sızmamalı (bkz.
	// internal/passwordreset paket yorumu).
	Request(ctx context.Context, email string)
	Confirm(ctx context.Context, rawToken, newPassword string) error
}

type passwordResetHandler struct {
	reset  PasswordResetService
	logger *slog.Logger
}

type passwordResetRequest struct {
	Email string `json:"email"`
}

type passwordResetConfirmRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// request — POST /v1/auth/password-reset
//
// HER DURUMDA AYNI 202: adres kayıtlı olsun olmasın, gövde de süre de aynı.
// 1.1'in giriş ucundaki kararın ("e-posta yok" ile "parola yanlış" birebir
// aynı cevap) bu akıştaki karşılığı.
func (h *passwordResetHandler) request(w http.ResponseWriter, r *http.Request) {
	var req passwordResetRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	// BİÇİM DOĞRULAMASI YAPILMIYOR ve bu bilinçli.
	//
	// 6.4'ün abonelik ucu biçimsiz adresi 422 ile ayırıyordu ve orada doğruydu:
	// ziyaretçi kendi adresini yazıyor, yazım hatasını görmesi gerekiyor ve o
	// bilgi kayıt durumu hakkında bir şey söylemiyor. BURADA farklı bir
	// hesap var: bu uç bir hesap kurtarma yüzeyi ve cevabı ne kadar tek
	// biçimliyse o kadar iyi. "Geçerli bir e-posta değil" demek, saldırgana
	// hangi girdilerin uca ULAŞTIĞINI söyleyerek elemesini kolaylaştırırdı.
	// Biçimsiz adres zaten hiçbir kullanıcıyla eşleşmiyor ve sessizce aynı
	// cevabı alıyor.
	h.reset.Request(r.Context(), req.Email)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": "Adres kayıtlıysa sıfırlama bağlantısı gönderildi.",
	})
}

// confirm — POST /v1/auth/password-reset/confirm
func (h *passwordResetHandler) confirm(w http.ResponseWriter, r *http.Request) {
	var req passwordResetConfirmRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	if req.Token == "" {
		writeFieldError(w, r, "token", "zorunlu")
		return
	}

	// PAROLA KURALLARI 1.1'İN KURALLARININ TA KENDİSİ: kayıt ucuyla aynı
	// doğrulayıcı çağrılıyor. İkinci bir kural seti, altı ay sonra "kayıtta
	// kabul edilen ama sıfırlamada reddedilen parola" demekti.
	v := &validator{}
	validatePassword(v, req.Password)
	if apiErr := v.err(); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	switch err := h.reset.Confirm(r.Context(), req.Token, req.Password); {
	case err == nil:
		// 204: yeni bir oturum AÇILMIYOR. Sıfırlama az önce kullanıcının TÜM
		// oturumlarını iptal etti; hemen ardından bir oturum açmak, o kararı
		// aynı istekte geri almak olurdu. Kullanıcı yeni parolasıyla giriş
		// yapıyor — ve böylece parolanın gerçekten değiştiğini de görüyor.
		w.WriteHeader(http.StatusNoContent)

	case errors.Is(err, passwordreset.ErrTokenNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound,
			"sıfırlama bağlantısı geçersiz veya daha önce kullanılmış")

	case errors.Is(err, passwordreset.ErrTokenExpired):
		// 410 (Gone), 404 DEĞİL: kullanıcının yapacağı iş farklı — yeni bir
		// bağlantı istemek (8.3'ün süresi dolmuş davetiyle aynı ayrım).
		writeError(w, r, http.StatusGone, codeResetTokenExpired,
			"sıfırlama bağlantısının süresi dolmuş; yenisini isteyin")

	default:
		writeInternalError(w, r, h.logger, "parola sıfırlanamadı", err)
	}
}
