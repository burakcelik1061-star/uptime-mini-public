// Package token, e-posta linklerinde taşınan TAHMİN EDİLEMEZ tek kullanımlık
// token'ları üretir ve saklama biçimlerini hesaplar (Faz 8.3, 8.4).
//
// NEDEN AYRI BİR PAKET: davet (8.3) ve parola sıfırlama (8.4) aynı üç kararı
// paylaşıyor — 256 bit entropi, `crypto/rand`, veritabanında HASH. Bu kodu iki
// pakette birden yazmak, ikisinin bir gün ayrışması (ör. birinin `math/rand`e
// düşmesi) demekti ve o ayrışma sessiz olurdu.
//
// 6.4'ÜN `internal/subscribers/token.go` DOSYASI YERİNDE KALDI ve bu bilinçli:
// orası yalnızca token üretmiyor, bir KARAR da kodluyor (onay token'ı hash'li,
// iptal token'ı HAM — çünkü her bildirimde çalışan bir iptal linki gerekiyor).
// Çalışan ve testli bir akışı 8.3'ün yolu üstünde yeniden yazmak, bu fazın
// kapsamı değildi. Aynı üç kararı taşıdığı için ileride buraya taşınabilir.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// entropyBytes, token'ın uzunluğu.
//
// 32 bayt = 256 bit: kaba kuvvetle tahmin edilmesi pratikte imkânsız (1.2'nin
// refresh token'ı ve 6.4'ün abonelik token'larıyla aynı büyüklük). base64url
// ile 43 karakter — bir e-posta linkine rahatça sığıyor ve satır kırılmıyor.
const entropyBytes = 32

// New, tahmin edilemez bir token üretir.
//
// `crypto/rand` ŞART: `math/rand` tohumu tahmin edilebilir bir üreteç ve
// buradaki token TEK BAŞINA bir kimlik doğrulama aracı — tahmin edilebilir
// olması, bir yabancının başkasının organizasyonuna katılması ya da
// başkasının parolasını sıfırlaması demek.
func New() (string, error) {
	raw := make([]byte, entropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("token üretilemedi: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Hash, token'ın veritabanında saklanan biçimini üretir.
//
// TOKEN HAM SAKLANMIYOR (1.2'nin refresh token kararının aynısı): veritabanı
// dökümünü ele geçiren biri, bekleyen davetleri kabul etme ya da bekleyen
// sıfırlama isteklerini kullanma yeteneğini HAZIR bulmasın. Ham değer
// yalnızca e-postadaki linkte var.
//
// HMAC DEĞİL DÜZ SHA-256 (6.4'ün gerekçesinin aynısı): HMAC'in sırrı, çalınan
// bir veritabanından yeni token ÜRETİLEMEMESİ içindir. Burada token zaten 256
// bit rastgele — sözlük saldırısı diye bir şey yok — ve ek bir sır,
// yapılandırmaya yönetilecek bir değer daha koyardı. Kaybı da ağır olurdu:
// sır değişirse bekleyen TÜM davet ve sıfırlama linkleri sessizce ölürdü.
func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}
