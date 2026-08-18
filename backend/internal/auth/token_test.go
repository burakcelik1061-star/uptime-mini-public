package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var testSecret = []byte("test-imzalama-anahtari-32-karakterden-uzun")

const testTTL = 15 * time.Minute

// newTestTokenService, saati sabitlenmiş bir servis döner: süre dolması
// gerçekten beklenmeden (time.Sleep olmadan) ölçülebilsin.
func newTestTokenService(t *testing.T, now *time.Time) *TokenService {
	t.Helper()

	s := NewTokenService(testSecret, testTTL)
	s.now = func() time.Time { return *now }

	return s
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)
	userID := uuid.New()

	token, err := svc.Issue(userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if token.ExpiresIn != int64(testTTL.Seconds()) {
		t.Errorf("expiresIn = %d, beklenen %d", token.ExpiresIn, int64(testTTL.Seconds()))
	}
	if !token.ExpiresAt.Equal(now.Add(testTTL)) {
		t.Errorf("expiresAt = %s, beklenen %s", token.ExpiresAt, now.Add(testTTL))
	}

	identity, err := svc.Verify(token.Value)
	if err != nil {
		t.Fatalf("token doğrulanamadı: %v", err)
	}
	if identity.UserID != userID {
		t.Errorf("userID = %s, beklenen %s", identity.UserID, userID)
	}
	if identity.TokenID == "" {
		t.Error("jti boş")
	}
}

// Aynı kullanıcı için AYNI ANDA üretilen iki token birebir aynı olmamalı.
// canny-mini'de refresh token'ın `jti` olmadan üretilmesi tam olarak bu
// yüzden gerçek bir açıktı: aynı saniyedeki iki token çakışıyordu.
func TestTokensAreUniqueEvenAtTheSameInstant(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)
	userID := uuid.New()

	first, err := svc.Issue(userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	second, err := svc.Issue(userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if first.Value == second.Value {
		t.Fatal("aynı anda üretilen iki token birebir aynı")
	}

	firstID, err := svc.Verify(first.Value)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	secondID, err := svc.Verify(second.Value)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if firstID.TokenID == secondID.TokenID {
		t.Error("iki token'ın jti değeri aynı")
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)

	token, err := svc.Issue(uuid.New())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	// Saati token'ın ömrünün bir saniye ötesine al.
	now = now.Add(testTTL + time.Second)

	if _, err := svc.Verify(token.Value); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("hata = %v, beklenen ErrTokenExpired", err)
	}
}

func TestVerifyAcceptsTokenUntilTheLastSecond(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)

	token, err := svc.Issue(uuid.New())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	now = now.Add(testTTL - time.Second)

	if _, err := svc.Verify(token.Value); err != nil {
		t.Fatalf("süresi dolmamış token reddedildi: %v", err)
	}
}

func TestVerifyRejectsTokenSignedWithAnotherSecret(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	issuer := newTestTokenService(t, &now)

	token, err := issuer.Issue(uuid.New())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	other := NewTokenService([]byte("bambaska-bir-imzalama-anahtari-32+"), testTTL)
	if _, err := other.Verify(token.Value); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("hata = %v, beklenen ErrTokenInvalid", err)
	}
}

// Algoritma karışıklığı (alg confusion): saldırgan "alg":"none" ile imzasız
// bir token gönderir. jwt.WithValidMethods bunu doğrulama başlamadan keser.
func TestVerifyRejectsNoneAlgorithm(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)

	forged, err := jwt.NewWithClaims(jwt.SigningMethodNone, accessClaims{
		TokenUse: tokenUseAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenAudience},
			Subject:   uuid.NewString(),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("test token'ı üretilemedi: %v", err)
	}

	if _, err := svc.Verify(forged); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("imzasız token kabul edildi (hata = %v)", err)
	}
}

// Doğru sırla imzalanmış ama BAŞKA amaç için üretilmiş bir token (Faz 1.2'nin
// refresh token'ı) access token yerine geçmemeli.
func TestVerifyRejectsTokenIssuedForAnotherUse(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)

	refresh, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		TokenUse: "refresh",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenAudience},
			Subject:   uuid.NewString(),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}).SignedString(testSecret)
	if err != nil {
		t.Fatalf("test token'ı üretilemedi: %v", err)
	}

	if _, err := svc.Verify(refresh); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("refresh token access olarak kabul edildi (hata = %v)", err)
	}
}

func TestVerifyRejectsWrongIssuerOrAudience(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)

	cases := map[string]jwt.RegisteredClaims{
		"başka issuer": {
			Issuer:    "baska-servis",
			Audience:  jwt.ClaimStrings{tokenAudience},
			Subject:   uuid.NewString(),
			ID:        uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		"başka audience": {
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{"baska-api"},
			Subject:   uuid.NewString(),
			ID:        uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		"exp yok": {
			Issuer:   tokenIssuer,
			Audience: jwt.ClaimStrings{tokenAudience},
			Subject:  uuid.NewString(),
			ID:       uuid.NewString(),
		},
		"sub uuid değil": {
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenAudience},
			Subject:   "kullanici-1",
			ID:        uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		"jti yok": {
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{tokenAudience},
			Subject:   uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}

	for name, claims := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
				TokenUse:         tokenUseAccess,
				RegisteredClaims: claims,
			}).SignedString(testSecret)
			if err != nil {
				t.Fatalf("test token'ı üretilemedi: %v", err)
			}

			if _, err := svc.Verify(raw); !errors.Is(err, ErrTokenInvalid) {
				t.Errorf("token kabul edildi (hata = %v)", err)
			}
		})
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	svc := newTestTokenService(t, &now)

	for _, raw := range []string{"", "abc", "a.b.c", "Bearer token"} {
		if _, err := svc.Verify(raw); err == nil {
			t.Errorf("%q kabul edildi", raw)
		}
	}
}
