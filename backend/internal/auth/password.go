// Package auth, kimlik doğrulamanın dile/HTTP'ye bağımlı olmayan çekirdeğini
// içerir: parola hash'leme, access token üretme/doğrulama ve kimliğin
// `context` ile taşınması. HTTP tarafı (handler, middleware) internal/httpapi
// içinde; bu paket net/http bilmiyor, böylece testleri saf ve hızlı.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Neden Argon2id, bcrypt değil:
//
//   - bcrypt parolayı 72 BYTE'ta sessizce kırpar. Yani "…72 byte" ile
//     "…72 byte + fazlası" AYNI hesaba giriş yapar. canny-mini bcrypt
//     kullanıyor ve bu sınırı ayrıca ele almak gerekiyordu; burada sınır
//     hiç doğmuyor.
//   - OWASP'ın güncel önerisi Argon2id; bcrypt yalnızca "eski sistem" başlığı
//     altında duruyor.
//   - Ek bağımlılık YOK: argon2 de bcrypt de golang.org/x/crypto modülünden
//     geliyor.
//   - Bellek-zor (memory-hard) bir fonksiyon: GPU/ASIC ile paralel kırma
//     bcrypt'e göre belirgin şekilde pahalı.

// Argon2Params, hash üretiminde kullanılan maliyet parametreleri.
//
// Değerler OWASP Password Storage Cheat Sheet'in Argon2id için verdiği
// minimum yapılandırmalardan biri: m=19 MiB, t=2, p=1. Daha yüksek bellek
// daha iyi ama tek bir giriş isteği o kadar bellek ayırıyor — 19 MiB, api
// konteynerinin bütçesiyle uyumlu bilinçli bir taban. (Eşzamanlı giriş
// denemeleriyle bellek tüketme saldırısının asıl cevabı hız sınırlama;
// Faz 7.4.)
type Argon2Params struct {
	// MemoryKiB, KiB cinsinden bellek maliyeti.
	MemoryKiB uint32
	// Iterations, zaman maliyeti (t).
	Iterations uint32
	// Parallelism, paralel şerit sayısı (p).
	Parallelism uint8
	// SaltLength, rastgele tuzun byte uzunluğu.
	SaltLength uint32
	// KeyLength, üretilen anahtarın byte uzunluğu.
	KeyLength uint32
}

// DefaultArgon2Params, üretimde kullanılan parametreler.
var DefaultArgon2Params = Argon2Params{
	MemoryKiB:   19456, // 19 MiB
	Iterations:  2,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

const (
	// MinPasswordRunes, kabul edilen en kısa parola (karakter sayısı).
	// NIST SP 800-63B ile uyumlu: uzunluk şartı var, "en az bir büyük harf
	// ve bir simge" gibi kompozisyon kuralları YOK (kullanıcıyı tahmin
	// edilebilir kalıplara itiyorlar).
	MinPasswordRunes = 8

	// MaxPasswordBytes, kabul edilen en uzun parola (BYTE).
	//
	// Argon2'nin böyle bir sınırı yok; bu sınır tamamen kaynak tüketimine
	// karşı: megabyte'lık bir gövde her istekte gereksiz iş demek.
	MaxPasswordBytes = 1024
)

var (
	// ErrPasswordTooLong, MaxPasswordBytes aşıldığında döner.
	ErrPasswordTooLong = errors.New("parola en fazla 1024 byte olabilir")
	// ErrPasswordMismatch, parola hash ile eşleşmediğinde döner.
	ErrPasswordMismatch = errors.New("parola eşleşmiyor")
	// ErrInvalidHash, saklanan hash dizesi çözümlenemediğinde döner.
	ErrInvalidHash = errors.New("parola hash'i çözümlenemedi")
)

// Hasher, parola hash'leme/doğrulama işini yapar.
//
// Params yalnızca YENİ hash üretirken kullanılır; doğrulama parametreleri
// hash'in kendisinden okur (bkz. Verify).
type Hasher struct {
	Params Argon2Params
}

// NewHasher, üretim parametreleriyle bir hasher üretir.
func NewHasher() Hasher {
	return Hasher{Params: DefaultArgon2Params}
}

// Hash, parolanın PHC biçimindeki Argon2id kodlamasını üretir:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<tuz-b64>$<hash-b64>
//
// Parametrelerin hash'in İÇİNDE olması bilinçli: maliyet ileride
// artırıldığında eski kayıtlar kendi parametreleriyle doğrulanmaya devam
// eder, kimsenin parolası geçersizleşmez.
func (h Hasher) Hash(password string) (string, error) {
	if len(password) > MaxPasswordBytes {
		return "", ErrPasswordTooLong
	}

	p := h.Params
	if p.SaltLength == 0 {
		p = DefaultArgon2Params
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("tuz üretilemedi: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify, parolanın saklanan hash ile eşleşip eşleşmediğini söyler.
//
// Parametreler h.Params'tan DEĞİL, hash dizesinden okunur: farklı (ör. daha
// eski ve daha ucuz) parametrelerle üretilmiş kayıtlar doğrulanmaya devam
// eder.
//
// Eşleşmezse ErrPasswordMismatch, hash bozuksa ErrInvalidHash döner —
// çağıran taraf ikisini de "geçersiz kimlik bilgisi" olarak cevaplamalı.
func (h Hasher) Verify(encodedHash, password string) error {
	if len(password) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}

	p, salt, want, err := decodeHash(encodedHash)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, p.KeyLength)

	// Byte byte karşılaştırma, ilk farkta dönerek doğru byte sayısını sızdırır.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}

	return nil
}

// VerifyDummy, email'e karşılık bir kullanıcı BULUNAMADIĞINDA çağrılır.
//
// Gerçek bir doğrulamayla aynı maliyetli işi yapar: aksi hâlde "kayıtlı
// olmayan email" isteği belirgin şekilde daha hızlı cevaplanır ve cevap
// süresi, hata gövdeleri birebir aynı olsa bile hangi email'lerin kayıtlı
// olduğunu ele verir.
func (h Hasher) VerifyDummy(password string) {
	// Hash üretmenin maliyeti doğrulamayla aynı: ikisi de tek bir
	// argon2.IDKey çağrısına dayanıyor.
	_, _ = h.Hash(password)
}

// Saklanan hash'ten okunan parametrelerin makul aralıkları. Hash kendi
// veritabanımızdan geliyor; yine de bozuk/kötü niyetli bir satırın süreci
// belleksiz bırakmasına izin verilmiyor.
const (
	maxDecodedMemoryKiB = 1 << 20 // 1 GiB
	maxDecodedIteration = 16
	maxDecodedKeyLength = 64
	maxDecodedSaltBytes = 64
)

func decodeHash(encoded string) (Argon2Params, []byte, []byte, error) {
	// "$argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>" → baştaki boş parça dahil
	// 6 parça.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}

	if parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, fmt.Errorf("%w: desteklenmeyen algoritma %q", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return Argon2Params{}, nil, nil, fmt.Errorf("%w: desteklenmeyen sürüm %d", ErrInvalidHash, version)
	}

	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	if p.MemoryKiB == 0 || p.MemoryKiB > maxDecodedMemoryKiB ||
		p.Iterations == 0 || p.Iterations > maxDecodedIteration ||
		p.Parallelism == 0 {
		return Argon2Params{}, nil, nil, fmt.Errorf("%w: parametreler makul aralıkta değil", ErrInvalidHash)
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) == 0 || len(salt) > maxDecodedSaltBytes {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}

	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) == 0 || len(key) > maxDecodedKeyLength {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))

	return p, salt, key, nil
}
