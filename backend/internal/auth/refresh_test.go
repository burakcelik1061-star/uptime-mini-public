package auth

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testRefreshFactory(ttl time.Duration) *RefreshTokenFactory {
	return NewRefreshTokenFactory([]byte("test-refresh-anahtari-32-karakterden-uzun"), ttl)
}

func TestRefreshTokenIsRandomAndDated(t *testing.T) {
	factory := testRefreshFactory(24 * time.Hour)
	before := time.Now()

	first, err := factory.New()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	second, err := factory.New()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if first.Value == second.Value {
		t.Fatal("iki token aynı değeri aldı")
	}
	if bytes.Equal(first.Hash, second.Hash) {
		t.Error("iki token aynı özeti aldı")
	}

	// 32 byte, base64url (padding'siz) → 43 karakter.
	if len(first.Value) != 43 {
		t.Errorf("token uzunluğu = %d, beklenen 43", len(first.Value))
	}
	if strings.ContainsAny(first.Value, "+/=") {
		t.Errorf("token url-güvenli değil: %q", first.Value)
	}

	if first.ExpiresAt.Before(before.Add(24*time.Hour)) || first.ExpiresAt.After(time.Now().Add(24*time.Hour)) {
		t.Errorf("expiresAt = %s, ~24 saat sonrası bekleniyordu", first.ExpiresAt)
	}
}

// Ham token hiçbir yerde saklanmıyor; veritabanına giden değer özet.
func TestRefreshTokenHashIsNotTheTokenItself(t *testing.T) {
	factory := testRefreshFactory(time.Hour)

	token, err := factory.New()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if bytes.Equal(token.Hash, []byte(token.Value)) {
		t.Fatal("özet ham token'ın kendisi")
	}
	if len(token.Hash) != 32 {
		t.Errorf("özet uzunluğu = %d, SHA-256 için 32 bekleniyordu", len(token.Hash))
	}
	if strings.Contains(string(token.Hash), token.Value) {
		t.Error("özet ham token'ı içeriyor")
	}
}

// Aynı token her seferinde aynı özeti vermeli (veritabanında aranabilsin).
func TestRefreshTokenHashIsDeterministic(t *testing.T) {
	factory := testRefreshFactory(time.Hour)

	token, err := factory.New()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if !bytes.Equal(factory.Hash(token.Value), token.Hash) {
		t.Fatal("aynı token farklı özet üretti")
	}
}

// Özet ANAHTARLI: veritabanı dökümü ele geçse bile, anahtarı bilmeyen biri
// elindeki token'ın hangi satıra karşılık geldiğini hesaplayamıyor.
func TestRefreshTokenHashDependsOnSecret(t *testing.T) {
	first := NewRefreshTokenFactory([]byte("birinci-anahtar-32-karakterden-uzun-olsun"), time.Hour)
	second := NewRefreshTokenFactory([]byte("ikinci-anahtar-32-karakterden-uzun-olsun!"), time.Hour)

	token, err := first.New()
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if bytes.Equal(first.Hash(token.Value), second.Hash(token.Value)) {
		t.Fatal("farklı anahtarlar aynı özeti üretti — anahtar kullanılmıyor")
	}
}
