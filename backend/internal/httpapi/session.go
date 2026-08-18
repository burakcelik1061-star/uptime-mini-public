package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/sessions"
)

// SessionService, oturum akışının davranışları (bkz. internal/sessions).
type SessionService interface {
	Start(ctx context.Context, userID uuid.UUID) (sessions.Issued, error)
	Rotate(ctx context.Context, rawToken string) (sessions.Rotated, error)
	Revoke(ctx context.Context, rawToken string) error
}

const (
	refreshCookieName = "uptime_refresh"

	// refreshCookiePath, cookie'nin gönderileceği yolları daraltıyor:
	// refresh token yalnızca /v1/auth altındaki uçlara gidiyor, her API
	// isteğinde ağda dolaşmıyor. Sızıntı yüzeyini küçülten bedava bir önlem.
	refreshCookiePath = "/v1/auth"
)

// setRefreshCookie, refresh token'ı cookie olarak yazar.
//
// Neden cookie (cevap gövdesi değil): `HttpOnly` sayesinde JavaScript
// okuyamıyor — XSS ile çalınan bir sayfada bile token dışarı sızmıyor.
// Access token ise tam tersine gövdede dönüyor ve istemcinin BELLEĞİNDE
// tutuluyor; cookie olmadığı için tarayıcı onu kendiliğinden hiçbir isteğe
// eklemiyor, yani API çağrılarında CSRF diye bir konu doğmuyor.
func setRefreshCookie(w http.ResponseWriter, token auth.RefreshToken, secure bool, now time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:  refreshCookieName,
		Value: token.Value,
		Path:  refreshCookiePath,
		// MaxAge saniye cinsinden; Expires eski tarayıcılar için.
		MaxAge:   int(token.ExpiresAt.Sub(now).Seconds()),
		Expires:  token.ExpiresAt,
		HttpOnly: true,
		// Secure yalnızca üretimde: local geliştirme http üzerinden koşuyor
		// ve Secure cookie orada hiç gönderilmezdi.
		Secure: secure,
		// SameSite=Strict: cookie'yi taşıyan tek uç yenileme/çıkış olduğu
		// için, başka bir sitenin tetiklediği isteklerde gönderilmemesi
		// CSRF'i kökten kapatıyor. Frontend (:3100) ile api (:3400) aynı
		// site sayıldığından meşru akış etkilenmiyor.
		SameSite: http.SameSiteStrictMode,
	})
}

// clearRefreshCookie, cookie'yi siler (logout ve geçersiz oturum).
func clearRefreshCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// refresh — POST /v1/auth/refresh
//
// Access token'ın ömrü dolduğunda istemci bu ucu çağırıyor. Sunulan refresh
// token KULLANILMIŞ olarak işaretlenip yerine yenisi veriliyor (rotation):
// böylece bir token'ın ağda/diskte kalan kopyası ikinci kez işe yaramıyor.
func (h *authHandler) refresh(w http.ResponseWriter, r *http.Request) {
	raw := refreshTokenFromRequest(r)
	if raw == "" {
		writeUnauthorized(w, r, codeInvalidRefreshToken, "oturum bulunamadı, yeniden giriş yapın")
		return
	}

	rotated, err := h.sessions.Rotate(r.Context(), raw)
	if err != nil {
		// Tanınmayan, süresi dolmuş, iptal edilmiş ve YENİDEN KULLANILMIŞ
		// token'ların hepsi AYNI cevabı alıyor. Reuse'u ayrı bir kodla
		// bildirmek, token'ı çalan tarafa "yakalandın" demekten başka işe
		// yaramazdı; meşru istemcinin yapacağı şey her hâlükârda aynı:
		// yeniden giriş. Ayrım sunucu logunda duruyor.
		if errors.Is(err, sessions.ErrInvalidRefreshToken) || errors.Is(err, sessions.ErrRefreshTokenReused) {
			clearRefreshCookie(w, h.secureCookies)
			writeUnauthorized(w, r, codeInvalidRefreshToken, "oturum geçersiz, yeniden giriş yapın")

			return
		}

		writeInternalError(w, r, h.logger, "oturum yenilenemedi", err)

		return
	}

	// Kullanıcı bilgisi token'dan değil veritabanından okunuyor (bkz. me).
	user, err := h.users.GetUserByID(r.Context(), rotated.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clearRefreshCookie(w, h.secureCookies)
			writeUnauthorized(w, r, codeInvalidRefreshToken, "oturum geçersiz, yeniden giriş yapın")

			return
		}

		writeInternalError(w, r, h.logger, "kullanıcı okunamadı", err)

		return
	}

	setRefreshCookie(w, rotated.Token, h.secureCookies, time.Now())

	access, err := h.tokens.Issue(user.ID)
	if err != nil {
		writeInternalError(w, r, h.logger, "access token üretilemedi", err)
		return
	}

	h.logger.Info("oturum yenilendi",
		slog.String("user_id", user.ID.String()),
		slog.String("session_id", rotated.SessionID.String()))

	writeJSON(w, http.StatusOK, authResponse{
		User:        newUserResponse(user),
		AccessToken: access.Value,
		ExpiresIn:   access.ExpiresIn,
	})
}

// logout — POST /v1/auth/logout
//
// Oturum ailesinin tamamını iptal eder ve cookie'yi siler. HER ZAMAN 204
// döner: "bu token sistemde yok" bilgisini vermenin faydası yok, çıkışın
// sonucu da her iki durumda aynı.
func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	if raw := refreshTokenFromRequest(r); raw != "" {
		if err := h.sessions.Revoke(r.Context(), raw); err != nil {
			writeInternalError(w, r, h.logger, "oturum kapatılamadı", err)
			return
		}
	}

	clearRefreshCookie(w, h.secureCookies)
	w.WriteHeader(http.StatusNoContent)
}

func refreshTokenFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil {
		return ""
	}

	return cookie.Value
}
