package auth

import (
	"errors"
	"strings"
	"testing"
)

// Testlerde ucuz parametreler: üretimdeki 19 MiB'lik maliyet her testte
// gereksiz yere ödenmesin. Doğruluk parametrelerden bağımsız.
func testHasher() Hasher {
	return Hasher{Params: Argon2Params{
		MemoryKiB:   64,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}}
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	h := testHasher()

	encoded, err := h.Hash("doğru-parola-123")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Errorf("hash PHC biçiminde değil: %q", encoded)
	}

	if err := h.Verify(encoded, "doğru-parola-123"); err != nil {
		t.Errorf("doğru parola doğrulanamadı: %v", err)
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	h := testHasher()

	encoded, err := h.Hash("doğru-parola-123")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if err := h.Verify(encoded, "yanlış-parola-123"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("hata = %v, beklenen ErrPasswordMismatch", err)
	}
}

// Aynı parola iki kez hash'lendiğinde farklı çıktı vermeli: aynı çıkıyorsa
// tuz kullanılmıyordur ve iki kullanıcının aynı parolayı seçtiği tablodan
// okunabilir hâle gelir.
func TestHashIsSaltedPerCall(t *testing.T) {
	h := testHasher()

	first, err := h.Hash("aynı-parola-123")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	second, err := h.Hash("aynı-parola-123")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if first == second {
		t.Fatal("aynı parola aynı hash'i üretti — tuz kullanılmıyor")
	}

	// Farklı tuzlara rağmen ikisi de doğrulanmalı.
	for i, encoded := range []string{first, second} {
		if err := h.Verify(encoded, "aynı-parola-123"); err != nil {
			t.Errorf("hash[%d] doğrulanamadı: %v", i, err)
		}
	}
}

// bcrypt'in 72-byte kırpması bu projede YOK: 72 byte'ı aşan bir parolanın
// ilk 72 byte'ı doğru olsa bile giriş REDDEDİLMELİ. bcrypt'te bu test
// başarısız olurdu (iki parola aynı hesaba girerdi) — Argon2id'ye geçmenin
// somut karşılığı bu.
func TestPasswordBeyondBcryptLimitIsNotTruncated(t *testing.T) {
	h := testHasher()

	base := strings.Repeat("a", 72)
	long := base + "FARKLI-KUYRUK"

	encoded, err := h.Hash(long)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if err := h.Verify(encoded, long); err != nil {
		t.Errorf("72 byte'tan uzun parola doğrulanamadı: %v", err)
	}

	if err := h.Verify(encoded, base); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("ilk 72 byte'ı doğru olan parola kabul edildi (hata = %v)", err)
	}

	if err := h.Verify(encoded, base+"BAŞKA-KUYRUK"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("72 byte'tan sonrası farklı olan parola kabul edildi (hata = %v)", err)
	}
}

func TestHashRejectsOversizedPassword(t *testing.T) {
	h := testHasher()

	if _, err := h.Hash(strings.Repeat("x", MaxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("hata = %v, beklenen ErrPasswordTooLong", err)
	}
}

func TestVerifyRejectsOversizedPassword(t *testing.T) {
	h := testHasher()

	encoded, err := h.Hash("doğru-parola-123")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if err := h.Verify(encoded, strings.Repeat("x", MaxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("hata = %v, beklenen ErrPasswordTooLong", err)
	}
}

// Parametreler hash'in içinde saklandığı için, DAHA ESKİ ve daha ucuz
// parametrelerle üretilmiş bir kayıt, maliyet artırıldıktan sonra da
// doğrulanabilmeli — aksi hâlde maliyeti yükseltmek herkesin parolasını
// geçersiz kılardı.
func TestVerifyAcceptsHashProducedWithDifferentParams(t *testing.T) {
	old := Hasher{Params: Argon2Params{MemoryKiB: 32, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}}
	current := testHasher()

	encoded, err := old.Hash("eski-kayıt-parolası")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if err := current.Verify(encoded, "eski-kayıt-parolası"); err != nil {
		t.Errorf("eski parametreli hash doğrulanamadı: %v", err)
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	h := testHasher()

	cases := map[string]string{
		"boş":                     "",
		"düz metin":               "parola",
		"eksik parça":             "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ",
		"fazla parça":             "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ$aGFzaA$fazla",
		"başka algoritma":         "$argon2i$v=19$m=64,t=1,p=1$c29tZXNhbHQ$aGFzaGhhc2hoYXNo",
		"desteklenmeyen sürüm":    "$argon2id$v=16$m=64,t=1,p=1$c29tZXNhbHQ$aGFzaGhhc2hoYXNo",
		"bozuk parametre metni":   "$argon2id$v=19$bellek=64$c29tZXNhbHQ$aGFzaGhhc2hoYXNo",
		"sıfır iterasyon":         "$argon2id$v=19$m=64,t=0,p=1$c29tZXNhbHQ$aGFzaGhhc2hoYXNo",
		"akıl dışı bellek":        "$argon2id$v=19$m=99999999,t=1,p=1$c29tZXNhbHQ$aGFzaGhhc2hoYXNo",
		"geçersiz base64 tuz":     "$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaGhhc2hoYXNo",
		"geçersiz base64 anahtar": "$argon2id$v=19$m=64,t=1,p=1$c29tZXNhbHQ$!!!",
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if err := h.Verify(encoded, "parola"); !errors.Is(err, ErrInvalidHash) {
				t.Errorf("hata = %v, beklenen ErrInvalidHash", err)
			}
		})
	}
}
