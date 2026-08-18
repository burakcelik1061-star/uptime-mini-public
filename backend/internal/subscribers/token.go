// Package subscribers, status page e-posta aboneliklerini yönetir (Faz 6.4).
//
// ZİYARETÇİ İLK KEZ YAZIYOR. 0.1 §2'nin "buradaki ziyaretçi hiçbir şey
// yazmıyor" kuralının BİLİNÇLİ İSTİSNASI: ziyaretçi yalnızca kendi e-posta
// adresini bırakıyor, ürünün verisine dokunmuyor.
//
// ABONE BİR KULLANICI DEĞİL (0.1'in kararı): parolası, oturumu, üyeliği yok.
// Kimliğini taşıyan tek şey e-postasına giden linklerdeki TAHMİN EDİLEMEZ
// token'lar — bu paketin en kritik parçası da o token'lar.
package subscribers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes, token'ın entropi uzunluğu.
//
// 32 bayt = 256 bit: tahmin edilmesi pratikte imkânsız (1.2'nin refresh
// token'ıyla aynı büyüklük). base64url ile 43 karakterlik bir dize oluyor,
// yani bir e-posta linkine rahatça sığıyor.
const tokenBytes = 32

// newToken, tahmin edilemez bir token üretir.
//
// `crypto/rand` ŞART: `math/rand` tohumu bilinen bir üreteç ve buradaki token
// tek başına bir kimlik doğrulama aracı — tahmin edilebilir olması, bir
// yabancının başkasının aboneliğini onaylayıp iptal edebilmesi demek.
func newToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("token üretilemedi: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashToken, token'ın veritabanında saklanan biçimini üretir.
//
// TOKEN HAM HÂLDE SAKLANMIYOR (1.2'nin refresh token kararının aynısı):
// veritabanını okuyabilen biri, aboneliği onaylama ya da başkasının
// aboneliğini iptal etme yeteneğini hazır bulmasın. Ham değer yalnızca
// e-postadaki linkte var.
//
// HMAC DEĞİL DÜZ SHA-256, ve bunun sebebi var: 1.2'de sır, çalınan bir
// veritabanından token ÜRETİLEMESİN diye vardı (refresh token'lar oturum
// devretmeye yarıyor). Burada token zaten 256 bit rastgele — sözlük saldırısı
// diye bir şey yok — ve ek bir sır, yapılandırmaya yönetilmesi gereken bir
// değer daha koyardı. Kaybı da ağır olurdu: sır değişirse TÜM abonelerin
// onay/iptal linkleri sessizce ölürdü.
func hashToken(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}
