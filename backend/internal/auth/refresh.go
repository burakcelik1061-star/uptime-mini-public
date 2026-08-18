package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"
)

// refreshTokenBytes, ham token'ın entropisi. 256 bit; tahmin edilmesi
// pratikte imkânsız, base64url ile 43 karakterlik bir dize oluyor.
const refreshTokenBytes = 32

// RefreshToken, üretilmiş bir refresh token'ın iki yüzü.
type RefreshToken struct {
	// Value, İSTEMCİYE giden ham değer. Sunucuda hiçbir yerde saklanmıyor.
	Value string
	// Hash, VERİTABANINA giden anahtarlı özet.
	Hash []byte
	// ExpiresAt, geçerlilik sonu.
	ExpiresAt time.Time
}

// RefreshTokenFactory, refresh token üretir ve özetler.
//
// Neden JWT DEĞİL: refresh token'ın iptal edilebilir olması gerekiyor
// (logout, reuse tespiti), bu da her kullanımda veritabanına bakmak demek.
// Veritabanına zaten bakılıyorsa imzanın taşıdığı bilgi bedava değil, sadece
// fazladan bir doğrulama yüzeyi (alg confusion, süresi dolmuş ama iptal
// edilmemiş token vb.) olurdu. Opak rastgele değer + veritabanı, hem daha
// küçük hem daha az varsayımlı.
type RefreshTokenFactory struct {
	// secret, HMAC anahtarı ("pepper"). Veritabanında DEĞİL, ortamda:
	// veritabanı dökümü tek başına ele geçse bile, satırlardaki özetlerden
	// kullanılabilir bir token üretilemiyor ve bir saldırgan elindeki
	// token'ın hangi satıra karşılık geldiğini hesaplayamıyor.
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewRefreshTokenFactory, verilen anahtar ve ömürle bir üretici kurar.
func NewRefreshTokenFactory(secret []byte, ttl time.Duration) *RefreshTokenFactory {
	return &RefreshTokenFactory{secret: secret, ttl: ttl, now: time.Now}
}

// TTL, üretilen token'ların ömrü (cookie süresi için).
func (f *RefreshTokenFactory) TTL() time.Duration { return f.ttl }

// New, yeni bir refresh token üretir.
func (f *RefreshTokenFactory) New() (RefreshToken, error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return RefreshToken{}, fmt.Errorf("refresh token üretilemedi: %w", err)
	}

	value := base64.RawURLEncoding.EncodeToString(raw)

	return RefreshToken{
		Value:     value,
		Hash:      f.Hash(value),
		ExpiresAt: f.now().Add(f.ttl),
	}, nil
}

// Hash, istemciden gelen ham token'ın veritabanında aranacak özetini üretir.
//
// Argon2 KULLANILMIYOR (parolalardan farkı burada): token 32 byte rastgele,
// yani sözlük/kaba kuvvet saldırısına konu olacak düşük entropili bir sır
// değil. Yavaş özet güvenlik katmaz, yalnızca her yenilemeyi pahalılaştırırdı.
// HMAC-SHA256 hem hızlı hem anahtarlı.
func (f *RefreshTokenFactory) Hash(value string) []byte {
	mac := hmac.New(sha256.New, f.secret)
	// hash.Hash'in Write'ı asla hata döndürmez (sözleşmesi böyle).
	_, _ = mac.Write([]byte(value))

	return mac.Sum(nil)
}
