package monitors

import (
	"net/url"
	"strings"
)

// MaxURLLength, hedef URL'in üst sınırı (şemadaki CHECK ile aynı).
const MaxURLLength = 2048

// normalizeURL, URL'i SAKLAMA biçimine getirir.
//
// Neden normalleştiriyoruz: aynı hedefi gösteren "HTTP://Example.COM:80/x" ile
// "http://example.com/x" iki ayrı kayıt gibi görünmesin ve kontrol eden kod
// her seferinde aynı dizeyi görsün. Ayrıştırılamayan girdi olduğu gibi geri
// dönüyor — hata mesajını doğrulama üretecek, normalleştirme değil.
func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)

	// Varsayılan port düşürülüyor: ":80" ile portsuz hâli aynı istek.
	switch {
	case parsed.Scheme == "http" && parsed.Port() == "80",
		parsed.Scheme == "https" && parsed.Port() == "443":
		parsed.Host = parsed.Hostname()
	}

	// Fragment (#bolum) SUNUCUYA HİÇ GÖNDERİLMEZ — tarayıcıda kalır. Kayıtta
	// tutmak, kullanıcıya kontrolün onu dikkate aldığını düşündürürdü.
	parsed.Fragment = ""
	parsed.RawFragment = ""

	return parsed.String()
}

// validateURL, hedef URL'in biçimini denetler.
//
// KAPSAM SINIRI: burada yalnızca BİÇİM doğrulanıyor. "Bu adrese istek atmak
// güvenli mi" sorusu (SSRF — localhost, 127.0.0.1, 169.254.169.254, özel ağ
// aralıkları, DNS'in iç adrese çözülmesi) BAŞKA bir sorudur ve Faz 7.4'e
// aittir. Oraya bırakılmasının sebebi sıra değil doğası: gerçek koruma
// ayrıştırma anında değil, İSTEK ATILIRKEN çözülen IP üzerinde yapılmalı
// (DNS rebinding). Reddedilen kontroller için hata tipi de şemada hazır
// bekliyor (`check_error_type.blocked_url`, migration 00001).
func validateURL(p *problems, raw string) {
	switch {
	case raw == "":
		p.fail("url", "zorunlu")
		return
	case len(raw) > MaxURLLength:
		p.fail("url", "en fazla 2048 karakter olabilir")
		return
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		p.fail("url", "geçerli bir URL olmalı")
		return
	}

	// Şema kontrolü ilk sırada: "example.com" yazan kullanıcının aldığı hata
	// "host yok" değil "http:// veya https:// ile başlamalı" olmalı
	// (url.Parse "example.com"u yol sanıyor).
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		p.fail("url", "http:// veya https:// ile başlamalı")
		return
	}

	if parsed.Host == "" {
		p.fail("url", "bir alan adı içermeli")
		return
	}

	if parsed.Hostname() == "" {
		p.fail("url", "bir alan adı içermeli")
		return
	}

	// "https://kullanici:parola@site" — kimlik bilgisi URL'de taşınırsa
	// monitor listesinde, loglarda ve hata mesajlarında görünür hâle gelir.
	// Kimlik doğrulama gereken uçlar için ileride ayrı bir alan gerekir.
	if parsed.User != nil {
		p.fail("url", "kullanıcı adı/parola içeremez")
		return
	}

	// Port yazılmışsa sayı olmalı; url.Parse "http://a:port/" gibi bir girdiyi
	// Host'a olduğu gibi koyuyor ve Port() boş dönüyor.
	if strings.Contains(parsed.Host, ":") && parsed.Port() == "" {
		p.fail("url", "port geçerli bir sayı olmalı")
	}
}
