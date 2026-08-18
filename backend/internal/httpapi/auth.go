package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// UserStore, auth uçlarının veritabanından ihtiyaç duyduğu davranışlar.
//
// Arayüz TÜKETEN tarafta tanımlı (sqlc'nin ürettiği `*store.Queries` onu
// kendiliğinden karşılıyor): handler testleri gerçek bir veritabanı olmadan
// koşabiliyor ve bu paket, ihtiyacı olmayan onlarca sorguya bağlanmıyor.
// `Pinger` ile aynı desen (bkz. health.go).
type UserStore interface {
	CreateUser(ctx context.Context, arg store.CreateUserParams) (store.User, error)
	GetUserByEmail(ctx context.Context, email string) (store.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
}

// usersEmailUniqueConstraint, migration'daki UNIQUE kısıtın Postgres'in
// verdiği varsayılan adı (tablo_sütun_key).
const usersEmailUniqueConstraint = "users_email_key"

type authHandler struct {
	users    UserStore
	hasher   auth.Hasher
	tokens   *auth.TokenService
	sessions SessionService
	logger   *slog.Logger
	// secureCookies, cookie'lerin yalnızca HTTPS üzerinden gönderilmesi
	// gerekip gerekmediği (üretimde evet, local http'de hayır).
	secureCookies bool
}

// userResponse, kullanıcının DIŞA VERİLEN gösterimi.
//
// store.User doğrudan serileştirilmiyor: o struct şemayı takip ediyor ve
// PasswordHash alanını taşıyor. Şemaya eklenen bir sütunun kazara API
// cevabına düşmesi, bu ayrı tip sayesinde mümkün değil.
type userResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// authResponse, kayıt ve giriş uçlarının ortak cevabı.
type authResponse struct {
	User        userResponse `json:"user"`
	AccessToken string       `json:"accessToken"`
	// ExpiresIn, saniye cinsinden. İstemci kendi saatine göre değil, bu
	// süreye göre tazeleme planlar.
	ExpiresIn int64 `json:"expiresIn"`
}

func newUserResponse(user store.User) userResponse {
	return userResponse{
		ID:        user.ID.String(),
		Email:     user.Email,
		Name:      user.Name,
		CreatedAt: user.CreatedAt,
	}
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// register — POST /v1/auth/register
//
// Not (bilinçli kabul): kayıt ucu, bir e-postanın sistemde olup olmadığını
// ele verir (409). Bunu gizlemenin tek gerçek yolu "kaydı e-posta ile
// onayla" akışı; bu projede dış e-posta servisi yok (bkz. docs/0.0-constraints.md
// GÜNCEL DURUM). GİRİŞ ucunda ise böyle bir sızıntı YOK — orada hem gövde
// hem süre aynı tutuluyor.
func (h *authHandler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	email := normalizeEmail(req.Email)
	name := strings.TrimSpace(req.Name)

	v := &validator{}
	validateEmail(v, email)
	validateName(v, name)
	validatePassword(v, req.Password)
	if apiErr := v.err(); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	hash, err := h.hasher.Hash(req.Password)
	if err != nil {
		writeInternalError(w, r, h.logger, "parola hash'lenemedi", err)
		return
	}

	user, err := h.users.CreateUser(r.Context(), store.CreateUserParams{
		Email:        email,
		PasswordHash: hash,
		Name:         name,
	})
	if err != nil {
		if postgres.IsUniqueViolation(err, usersEmailUniqueConstraint) {
			writeError(w, r, http.StatusConflict, codeEmailTaken, "bu e-posta adresi zaten kayıtlı")
			return
		}

		writeInternalError(w, r, h.logger, "kullanıcı oluşturulamadı", err)
		return
	}

	h.logger.Info("kullanıcı kaydoldu", slog.String("user_id", user.ID.String()))

	h.respondWithSession(w, r, user, http.StatusCreated)
}

// login — POST /v1/auth/login
func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	email := normalizeEmail(req.Email)

	// Girişte alan alan doğrulama YAPILMIYOR: "e-posta biçimi geçersiz"
	// demek, o adresin kayıtlı olup olmadığını sorgulayan birine yardım
	// etmekten başka bir işe yaramaz. Tek sınır, hash'lemeye bile gerek
	// olmayan kaba boyut kontrolü.
	if email == "" || req.Password == "" || len(req.Password) > auth.MaxPasswordBytes {
		writeInvalidCredentials(w, r)
		return
	}

	user, err := h.users.GetUserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Kullanıcı yok: yine de bir hash hesapla. Aksi hâlde bu cevap
			// belirgin şekilde daha hızlı döner ve gövdeler birebir aynı olsa
			// bile süre farkı "bu e-posta kayıtlı" bilgisini verir.
			h.hasher.VerifyDummy(req.Password)
			writeInvalidCredentials(w, r)

			return
		}

		writeInternalError(w, r, h.logger, "kullanıcı okunamadı", err)

		return
	}

	if err := h.hasher.Verify(user.PasswordHash, req.Password); err != nil {
		// Bozuk hash (ErrInvalidHash) da buraya düşer ve dışarıya "geçersiz
		// kimlik bilgisi" olarak görünür; ayrıntı loga gider.
		if errors.Is(err, auth.ErrInvalidHash) {
			h.logger.Error("saklanan parola hash'i çözümlenemedi",
				slog.String("user_id", user.ID.String()),
				slog.String("error", err.Error()))
		}

		writeInvalidCredentials(w, r)

		return
	}

	h.logger.Info("giriş yapıldı", slog.String("user_id", user.ID.String()))

	h.respondWithSession(w, r, user, http.StatusOK)
}

// me — GET /v1/me
//
// Token'ın taşıdığı kimliğin gerçekten kullanılabildiğini gösteren en küçük
// uç. Kullanıcı bilgisi token'dan DEĞİL veritabanından okunuyor: token
// üretildikten sonra değişen (veya silinen) bir kaydın eski hâliyle
// dolaşmaması için.
func (h *authHandler) me(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		// Buraya düşmek, rotanın requireAuth olmadan bağlandığı anlamına
		// gelir — sessiz kalmak yerine loglanıyor.
		h.logger.Error("kimlik doğrulanmamış istek korumalı uca ulaştı",
			slog.String("path", r.URL.Path))
		writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")

		return
	}

	user, err := h.users.GetUserByID(r.Context(), identity.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Token geçerli ama kullanıcı artık yok (ör. hesap silinmiş).
			writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")
			return
		}

		writeInternalError(w, r, h.logger, "kullanıcı okunamadı", err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]userResponse{"user": newUserResponse(user)})
}

// respondWithSession, kayıt/giriş cevabını yazar: gövdede kısa ömürlü access
// token, httpOnly cookie'de uzun ömürlü refresh token.
func (h *authHandler) respondWithSession(w http.ResponseWriter, r *http.Request, user store.User, status int) {
	issued, err := h.sessions.Start(r.Context(), user.ID)
	if err != nil {
		writeInternalError(w, r, h.logger, "oturum açılamadı", err)
		return
	}

	token, err := h.tokens.Issue(user.ID)
	if err != nil {
		writeInternalError(w, r, h.logger, "access token üretilemedi", err)
		return
	}

	// Cookie gövdeden ÖNCE yazılıyor: WriteHeader'dan sonra eklenen başlık
	// cevaba girmez.
	setRefreshCookie(w, issued.Token, h.secureCookies, time.Now())

	writeJSON(w, status, authResponse{
		User:        newUserResponse(user),
		AccessToken: token.Value,
		ExpiresIn:   token.ExpiresIn,
	})
}

// writeInvalidCredentials, "e-posta yok" ve "parola yanlış" için TEK cevap.
// İkisini ayırmak, saldırgana hangi e-postaların kayıtlı olduğunu sayfa
// sayfa toplama imkânı verirdi.
func writeInvalidCredentials(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusUnauthorized, codeInvalidCredentials, "e-posta veya parola hatalı")
}

func writeUnauthorized(w http.ResponseWriter, r *http.Request, code, message string) {
	// RFC 9110: 401 cevabı istemciye hangi şemayla kimlik doğrulayacağını
	// söylemeli.
	w.Header().Set("WWW-Authenticate", `Bearer realm="uptime-mini"`)
	writeError(w, r, http.StatusUnauthorized, code, message)
}

func validatePassword(v *validator, password string) {
	switch {
	case password == "":
		v.fail("password", "zorunlu")
	case utf8.RuneCountInString(password) < auth.MinPasswordRunes:
		v.fail("password", fmt.Sprintf("en az %d karakter olmalı", auth.MinPasswordRunes))
	case len(password) > auth.MaxPasswordBytes:
		// BYTE sınırı: Argon2'nin bir uzunluk sınırı yok, bu sınır yalnızca
		// kaynak tüketimine karşı. (bcrypt'in 72 byte'ta SESSİZCE kırpması
		// gibi bir tuzak burada yok — bkz. internal/auth/password.go.)
		v.fail("password", fmt.Sprintf("en fazla %d byte olabilir", auth.MaxPasswordBytes))
	}
}

// requireAuth, `Authorization: Bearer <token>` başlığını doğrular ve kimliği
// context'e yazar.
//
// Rotaya tek tek değil, chi alt-router'ına TAKILIYOR (bkz. router.go):
// korumalı bir ucun middleware'i takmayı unutmasıyla açıkta kalması,
// canny-mini'de yaşanmış bir sınıf hata.
func requireAuth(tokens *auth.TokenService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeUnauthorized(w, r, codeUnauthorized, "kimlik doğrulaması gerekli")
				return
			}

			identity, err := tokens.Verify(raw)
			if err != nil {
				if errors.Is(err, auth.ErrTokenExpired) {
					// Ayrı kod: istemci "yeniden giriş yap" yerine token
					// tazeleyebileceğini bilsin (Faz 1.2).
					writeUnauthorized(w, r, codeTokenExpired, "token süresi dolmuş")
					return
				}

				writeUnauthorized(w, r, codeUnauthorized, "token geçersiz")

				return
			}

			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
		})
	}
}

func bearerToken(header string) (string, bool) {
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}

	return value, true
}
