package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/sessions"
)

// fakeSessionService, oturum servisinin HTTP katmanı için sahtesi.
//
// Rotasyonun ASIL mantığı (satır kilidi, yeniden kullanım tespiti, aile
// iptali) burada DEĞİL, internal/sessions'ın gerçek veritabanına karşı koşan
// testlerinde ölçülüyor. Buradaki testlerin konusu HTTP tarafı: cookie
// bayrakları, durum kodları, hata gövdesi.
type fakeSessionService struct {
	factory *auth.RefreshTokenFactory

	startedFor []uuid.UUID
	revoked    []string

	rotateUserID uuid.UUID
	rotateErr    error
	startErr     error
	revokeErr    error
}

func newFakeSessionService() *fakeSessionService {
	return &fakeSessionService{
		factory: auth.NewRefreshTokenFactory([]byte(testJWTSecret+"-refresh"), 30*24*time.Hour),
	}
}

func (f *fakeSessionService) Start(_ context.Context, userID uuid.UUID) (sessions.Issued, error) {
	if f.startErr != nil {
		return sessions.Issued{}, f.startErr
	}

	f.startedFor = append(f.startedFor, userID)

	token, err := f.factory.New()
	if err != nil {
		return sessions.Issued{}, err
	}

	return sessions.Issued{SessionID: uuid.New(), Token: token}, nil
}

func (f *fakeSessionService) Rotate(_ context.Context, _ string) (sessions.Rotated, error) {
	if f.rotateErr != nil {
		return sessions.Rotated{}, f.rotateErr
	}

	token, err := f.factory.New()
	if err != nil {
		return sessions.Rotated{}, err
	}

	return sessions.Rotated{SessionID: uuid.New(), UserID: f.rotateUserID, Token: token}, nil
}

func (f *fakeSessionService) Revoke(_ context.Context, rawToken string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}

	f.revoked = append(f.revoked, rawToken)

	return nil
}

// findCookie, cevaptaki Set-Cookie başlıklarından adı verilen cookie'yi bulur.
func findCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}

	return nil
}

func withRefreshCookie(value string) map[string]string {
	return map[string]string{"Cookie": refreshCookieName + "=" + value}
}

// --- Cookie sözleşmesi -------------------------------------------------------

func TestLoginSetsHardenedRefreshCookie(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	cookie := findCookie(t, rec, refreshCookieName)
	if cookie == nil {
		t.Fatal("refresh cookie yazılmadı")
	}

	if !cookie.HttpOnly {
		t.Error("cookie HttpOnly değil — XSS ile okunabilir olurdu")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, beklenen Strict", cookie.SameSite)
	}
	if cookie.Path != refreshCookiePath {
		t.Errorf("Path = %q, beklenen %q", cookie.Path, refreshCookiePath)
	}
	if cookie.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, pozitif olmalı", cookie.MaxAge)
	}
	if cookie.Value == "" {
		t.Error("cookie değeri boş")
	}

	// Refresh token GÖVDEDE dönmemeli: cookie'nin HttpOnly olmasının anlamı
	// kalmazdı.
	if strings.Contains(rec.Body.String(), cookie.Value) {
		t.Error("refresh token cevap gövdesinde de dönüyor")
	}
}

// Secure bayrağı üretimde açık, local http'de kapalı olmalı (aksi hâlde
// cookie hiç gönderilmezdi).
func TestSecureCookieFlagFollowsConfiguration(t *testing.T) {
	for name, secure := range map[string]bool{"üretim": true, "geliştirme": false} {
		t.Run(name, func(t *testing.T) {
			deps := newTestDeps(t)
			deps.SecureCookies = secure
			router := NewRouter(deps)

			rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
			cookie := findCookie(t, rec, refreshCookieName)
			if cookie == nil {
				t.Fatal("refresh cookie yazılmadı")
			}
			if cookie.Secure != secure {
				t.Errorf("Secure = %v, beklenen %v", cookie.Secure, secure)
			}
		})
	}
}

// --- Yenileme ----------------------------------------------------------------

func TestRefreshRotatesCookieAndReturnsNewAccessToken(t *testing.T) {
	router, users, sessionSvc := newAuthTestRouterWithSessions(t)

	register := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
	first := findCookie(t, register, refreshCookieName)
	registered := decodeAuthResponse(t, register)

	// Sahte servis, yenilemede bu kullanıcıyı döndürecek.
	sessionSvc.rotateUserID = users.byEmail[testEmail].ID

	rec := do(t, router, http.MethodPost, "/v1/auth/refresh", "", withRefreshCookie(first.Value))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	second := findCookie(t, rec, refreshCookieName)
	if second == nil {
		t.Fatal("yenilemede cookie yazılmadı")
	}
	if second.Value == first.Value {
		t.Error("cookie değişmedi — rotasyon istemciye yansımıyor")
	}

	body := decodeAuthResponse(t, rec)
	if body.AccessToken == "" {
		t.Error("accessToken boş")
	}
	if body.User.ID != registered.User.ID {
		t.Errorf("kullanıcı = %s, beklenen %s", body.User.ID, registered.User.ID)
	}
}

func TestRefreshWithoutCookieIsRejected(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	rec := do(t, router, http.MethodPost, "/v1/auth/refresh", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401", rec.Code)
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeInvalidRefreshToken {
		t.Errorf("code = %q, beklenen %q", code, codeInvalidRefreshToken)
	}
}

// Geçersiz, süresi dolmuş, iptal edilmiş ve YENİDEN KULLANILMIŞ token'ların
// hepsi aynı cevabı almalı: reuse'u ayrı bir kodla bildirmek, token'ı çalana
// "yakalandın" demekten başka işe yaramaz.
func TestRefreshFailuresAreIndistinguishableAndClearCookie(t *testing.T) {
	for name, failure := range map[string]error{
		"geçersiz token":   sessions.ErrInvalidRefreshToken,
		"yeniden kullanım": sessions.ErrRefreshTokenReused,
	} {
		t.Run(name, func(t *testing.T) {
			router, _, sessionSvc := newAuthTestRouterWithSessions(t)
			sessionSvc.rotateErr = failure

			rec := do(t, router, http.MethodPost, "/v1/auth/refresh", "", withRefreshCookie("eski-token"))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
			}

			body := decodeErrorResponse(t, rec).Error
			if body.Code != codeInvalidRefreshToken {
				t.Errorf("code = %q", body.Code)
			}
			if body.Message != "oturum geçersiz, yeniden giriş yapın" {
				t.Errorf("mesaj = %q", body.Message)
			}

			// Cookie temizlenmeli: tarayıcıda işe yaramaz bir değer kalmasın.
			cookie := findCookie(t, rec, refreshCookieName)
			if cookie == nil {
				t.Fatal("cookie temizlenmedi")
			}
			if cookie.Value != "" || cookie.MaxAge >= 0 {
				t.Errorf("cookie silinmemiş: value=%q maxAge=%d", cookie.Value, cookie.MaxAge)
			}
		})
	}
}

func TestRefreshFailsClosedOnDatabaseError(t *testing.T) {
	router, _, sessionSvc := newAuthTestRouterWithSessions(t)
	sessionSvc.rotateErr = errors.New("bağlantı koptu")

	rec := do(t, router, http.MethodPost, "/v1/auth/refresh", "", withRefreshCookie("token"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bağlantı koptu") {
		t.Error("iç hata metni istemciye sızdı")
	}
}

// --- Çıkış -------------------------------------------------------------------

func TestLogoutRevokesSessionAndClearsCookie(t *testing.T) {
	router, _, sessionSvc := newAuthTestRouterWithSessions(t)

	register := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
	cookie := findCookie(t, register, refreshCookieName)

	rec := do(t, router, http.MethodPost, "/v1/auth/logout", "", withRefreshCookie(cookie.Value))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, beklenen 204 (%s)", rec.Code, rec.Body.String())
	}

	if len(sessionSvc.revoked) != 1 || sessionSvc.revoked[0] != cookie.Value {
		t.Errorf("iptal edilen token = %v, beklenen %q", sessionSvc.revoked, cookie.Value)
	}

	cleared := findCookie(t, rec, refreshCookieName)
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("cookie silinmedi: %+v", cleared)
	}
}

// Cookie'siz çıkış da başarılı sayılmalı: sonuç aynı ("oturum yok") ve
// "böyle bir token yok" bilgisi verilmiyor.
func TestLogoutWithoutCookieStillSucceeds(t *testing.T) {
	router, _, sessionSvc := newAuthTestRouterWithSessions(t)

	rec := do(t, router, http.MethodPost, "/v1/auth/logout", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, beklenen 204", rec.Code)
	}
	if len(sessionSvc.revoked) != 0 {
		t.Errorf("cookie yokken iptal denendi: %v", sessionSvc.revoked)
	}
}

// Oturum açılamıyorsa kayıt/giriş de başarısız olmalı: elinde refresh
// token'ı olmayan bir "başarılı giriş" cevabı, 15 dakika sonra sessizce
// oturumu düşerdi.
func TestRegisterFailsWhenSessionCannotBeStarted(t *testing.T) {
	router, users, sessionSvc := newAuthTestRouterWithSessions(t)
	sessionSvc.startErr = errors.New("veritabanı yok")

	rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
	if findCookie(t, rec, refreshCookieName) != nil {
		t.Error("oturum açılamadığı hâlde cookie yazıldı")
	}
	if _, ok := users.byEmail[testEmail]; !ok {
		t.Error("kullanıcı oluşturulmuş olmalıydı (hata oturum aşamasında)")
	}
}
