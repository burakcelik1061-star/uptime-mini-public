package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// tokenIssuer / tokenAudience, token'ın nereden çıktığını ve nerede
	// geçerli olduğunu sabitler. Doğrulamada ikisi de ZORUNLU kontrol:
	// ileride ikinci bir imzalayan/tüketici olduğunda (ör. worker'ın
	// kendi token'ı) yanlış yerde üretilmiş bir token sessizce kabul
	// edilmesin.
	tokenIssuer   = "uptime-mini"
	tokenAudience = "uptime-mini-api"

	// tokenUseAccess, bu token'ın ne için olduğunu söyler. Faz 1.2'de refresh
	// token gelince, refresh token'ın access token yerine kullanılmasını
	// (ve tersini) engelleyen şey bu alan olacak — iki token'ın sırları da
	// ayrı ama tek savunmaya güvenilmiyor.
	tokenUseAccess = "access"

	// signingMethodHS256, kabul edilen TEK imza algoritması.
	signingMethodHS256 = "HS256"
)

var (
	// ErrTokenInvalid, token çözümlenemediğinde/imzası tutmadığında döner.
	ErrTokenInvalid = errors.New("token geçersiz")
	// ErrTokenExpired, token'ın süresi dolduğunda döner. Ayrı bir hata:
	// istemci "yeniden giriş yap" ile "token'ı tazele" arasında ayrım
	// yapabilsin (Faz 1.2).
	ErrTokenExpired = errors.New("token süresi dolmuş")
)

// AccessToken, giriş/kayıt cevabında dönen kısa ömürlü token.
type AccessToken struct {
	Value     string
	ExpiresAt time.Time
	// ExpiresIn, saniye cinsinden kalan süre (istemcinin saatine güvenmeden
	// tazeleme zamanlaması yapabilmesi için).
	ExpiresIn int64
}

// accessClaims, token'ın gövdesi.
//
// Alan adı "typ" DEĞİL "token_use": "typ" JWT'nin BAŞLIK alanlarından biri,
// aynı adı gövdede kullanmak okuyanı yanıltırdı.
type accessClaims struct {
	TokenUse string `json:"token_use"`
	jwt.RegisteredClaims
}

// TokenService, access token üretir ve doğrular.
type TokenService struct {
	secret []byte
	ttl    time.Duration
	// now, testlerin saati ileri/geri alabilmesi için. Üretimde time.Now.
	now func() time.Time
}

// NewTokenService, verilen imzalama anahtarı ve ömürle bir servis kurar.
func NewTokenService(secret []byte, ttl time.Duration) *TokenService {
	return &TokenService{secret: secret, ttl: ttl, now: time.Now}
}

// Issue, kullanıcı için imzalı bir access token üretir.
func (s *TokenService) Issue(userID uuid.UUID) (AccessToken, error) {
	now := s.now()
	expiresAt := now.Add(s.ttl)

	claims := accessClaims{
		TokenUse: tokenUseAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   tokenIssuer,
			Audience: jwt.ClaimStrings{tokenAudience},
			Subject:  userID.String(),
			// jti: her token benzersiz. Aynı saniyede aynı kullanıcı için
			// üretilen iki token'ın BİREBİR aynı dizeye düşmesi (canny-mini'de
			// refresh token'da yaşanan gerçek hata) burada baştan imkânsız;
			// ayrıca tekil iptal (Faz 1.2) bu kimliğe dayanacak.
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return AccessToken{}, fmt.Errorf("token imzalanamadı: %w", err)
	}

	return AccessToken{
		Value:     signed,
		ExpiresAt: expiresAt,
		ExpiresIn: int64(s.ttl.Seconds()),
	}, nil
}

// Verify, token'ı doğrular ve içindeki kimliği döner.
func (s *TokenService) Verify(raw string) (Identity, error) {
	parser := jwt.NewParser(
		// EN KRİTİK SEÇENEK: algoritma karışıklığı (alg confusion) saldırısı,
		// başlıkta "alg":"none" veya asimetrik bir algoritma göndererek
		// imzayı atlatmaya dayanır. İzin verilen algoritma listesi tek
		// elemanlı olduğunda saldırı doğrulama başlamadan biter.
		jwt.WithValidMethods([]string{signingMethodHS256}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithAudience(tokenAudience),
		// exp'siz (süresiz) token kabul edilmez.
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		// Saat toleransı YOK: api ve istemci aynı makinede/ağda; tolerans
		// süresi dolmuş bir token'ın kabul edildiği bir pencere demek.
		jwt.WithLeeway(0),
		jwt.WithTimeFunc(s.now),
	)

	var claims accessClaims
	if _, err := parser.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return s.secret, nil
	}); err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Identity{}, ErrTokenExpired
		}
		return Identity{}, fmt.Errorf("%w: %s", ErrTokenInvalid, err)
	}

	if claims.TokenUse != tokenUseAccess {
		return Identity{}, fmt.Errorf("%w: token türü %q", ErrTokenInvalid, claims.TokenUse)
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: sub bir uuid değil", ErrTokenInvalid)
	}

	if claims.ID == "" {
		return Identity{}, fmt.Errorf("%w: jti yok", ErrTokenInvalid)
	}

	return Identity{UserID: userID, TokenID: claims.ID}, nil
}
