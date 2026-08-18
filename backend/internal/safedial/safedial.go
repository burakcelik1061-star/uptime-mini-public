// Package safedial, sunucunun KULLANICININ YAZDIĞI bir adrese istek atarken
// iç ağa ulaşmasını engeller (SSRF koruması — 7.4).
//
// Bu projenin en önemli güvenlik konusu, çünkü tasarımın kendisi bir SSRF
// makinesi: kullanıcı keyfi bir URL giriyor (monitor hedefi, webhook adresi)
// ve sunucu ona düzenli olarak istek atıyor. Koruma olmasaydı bir kullanıcı
// `http://localhost:5432`, `http://10.0.0.5/admin` ya da bulut sağlayıcının
// kimlik uçlarını (`http://169.254.169.254/latest/meta-data/`) hedef gösterip
// sunucuyu kendi iç ağında bir tarayıcı gibi kullanabilirdi. Cevabın
// GÖVDESİ dönmese bile durum kodu ve yanıt süresi bir yan kanal: "bu port
// açık mı" sorusu böyle cevaplanır.
//
// # Neden `net.Dialer.Control`
//
// Alternatifi URL'i (ya da çözülen IP'yi) İSTEKTEN ÖNCE denetlemek olurdu.
// Üç yerden birden sızardı:
//
//  1. **DNS rebinding.** Ad, denetim anında dış bir IP'ye, bağlantı anında
//     `127.0.0.1`e çözülür (TTL 0 ile saldırganın kendi sunucusu bunu kolayca
//     yapar). Denetim ile bağlantı arasındaki pencere tam olarak saldırının
//     yeri. `Control` bağlantı kurulmadan HEMEN ÖNCE, ÇÖZÜLMÜŞ adresle
//     çalışıyor — pencere kapanıyor.
//  2. **Yönlendirme.** Dış görünen bir adres 302 ile `169.254.169.254`e
//     yönlendirebilir (checker.go'daki uyarı tam olarak buydu). Her durak
//     YENİ bir bağlantı demek, yani her durak `Control`dan geçiyor: kontrol
//     ilk URL'e değil, gerçekten gidilen HER adrese uygulanıyor.
//  3. **Çok adresli isim.** Bir ad hem dış hem iç IP döndürebilir ve Go
//     hangisine bağlanacağını (Happy Eyeballs) kendi seçer. `Control`
//     seçilen adresi görüyor.
//
// # Gömülü IPv4 (7.5)
//
// Birden çok IPv6 yazımı bir IPv4 adresini gösterimin İÇİNE gömüyor —
// IPv4-mapped (`::ffff:`), IPv4-compatible (`::a.b.c.d`), 6to4 (`2002::/16`),
// Teredo (`2001::/32`), NAT64 (`64:ff9b::/96`) — ve hiçbiri `IsLoopback`
// ya da `IsPrivate` yüklemlerine takılmıyor. Her birini engellenen aralıklar
// listesine yazmak, listenin ilkesini ("özel olanı say değil, genel olmayanı
// reddet") ikinci kez ihlal etmek ve yarın çıkacak biçimi yine açıkta
// bırakmak olurdu. Kural bu yüzden TEK: gömülü adres çıkarılıp AYNI kontrol
// ona da uygulanıyor (bkz. embeddedIPv4s).
//
// # Kapı: yalnızca development/test
//
// Yerelde monitor hedefi çoğu zaman `localhost` (ve 7.2'nin E2E'si tam olarak
// öyle çalışıyor). `AllowPrivate` bu yüzden var ama YALNIZCA
// `APP_ENV=development|test` iken açılabiliyor (bkz. internal/config) —
// üretimde bayrağı set etmek bir şey değiştirmiyor. Aynı disiplin 1.1'in
// imzalama anahtarı üretiminde de uygulanıyor.
package safedial

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlocked, hedef adresin engellendiğini bildirir.
//
// Çağıranlar bunu `errors.Is` ile tanıyıp kendi hata tiplerine çeviriyor
// (checker → `blocked_url`). Metin kullanıcıya da gidebildiği için İÇ AĞ
// TOPOLOJİSİNİ ANLATMIYOR: hangi aralığa düştüğünü söylemek, sunucunun
// hangi ağda olduğunu dışarıya haber vermek olurdu.
var ErrBlocked = errors.New("hedef adres izin verilmeyen bir ağa ait")

// Policy, hangi adreslere bağlanılabileceğini belirler.
type Policy struct {
	// AllowPrivate açıkken loopback/özel/link-local adreslere de izin
	// veriliyor. Yalnızca development ve test ortamlarında set edilebiliyor.
	AllowPrivate bool
}

// Dialer, bu politikayı uygulayan bir çevirici döner.
//
// `Control` her bağlantı denemesinde, socket açıldıktan sonra ama BAĞLANTI
// KURULMADAN önce çağrılıyor; hata döndürmek bağlantıyı hiç kurdurmuyor.
func (p Policy) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		// KeepAlive kapalı: kontrol istemcisi bağlantı yeniden kullanmıyor
		// (2.2'nin kararı).
		KeepAlive: -1,
		Control:   p.Control,
	}
}

// Control, `net.Dialer.Control` imzasına uyar.
//
// `address` çözülmüş bir "ip:port" — isim ÇÖZÜMÜ ÇOKTAN BİTMİŞ durumda,
// dolayısıyla burada ikinci bir DNS sorgusu yok ve rebinding penceresi
// kapalı.
func (p Policy) Control(network, address string, _ syscall.RawConn) error {
	// Yalnızca TCP: uygulama HTTP konuşuyor. `unix` soketi ya da başka bir
	// ağ tipi buraya düşerse bilinmeyen bir yol açılmış demektir.
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return fmt.Errorf("%w: %s", ErrBlocked, network)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: adres çözümlenemedi", ErrBlocked)
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Buraya düşmek "çözülmemiş bir ad geldi" demek. İzin vermek,
		// korumanın tamamen atlanabildiği bir yol bırakırdı.
		return fmt.Errorf("%w: adres bir IP değil", ErrBlocked)
	}

	if err := p.check(addr); err != nil {
		return err
	}

	return nil
}

// check, tek bir adresi denetler.
func (p Policy) check(addr netip.Addr) error {
	// IPv4-mapped IPv6 (`::ffff:127.0.0.1`) SARILIŞINDAN ÇIKARILIYOR:
	// çıkarılmasaydı `IsLoopback()` false döner ve loopback bu yazımla
	// atlanabilirdi — klasik bir atlatma.
	addr = addr.Unmap()

	if p.AllowPrivate {
		return nil
	}

	if blocked, why := blockedRange(addr); blocked {
		// Sebep LOGA gidiyor (çağıran sarmalıyor), kullanıcıya giden metin
		// yine ErrBlocked'ın kendisi.
		return fmt.Errorf("%w (%s)", ErrBlocked, why)
	}

	// GÖMÜLÜ IPv4: AYNI KONTROL, ÇIKARILMIŞ ADRES ÜZERİNDE.
	//
	// Birden çok IPv6 yazımı bir IPv4 adresini gösterimin İÇİNE gömüyor ve
	// hiçbiri `IsLoopback`/`IsPrivate` gibi yüklemlere takılmıyor. Bunları
	// engellenen aralıklar listesine tek tek eklemek dosyanın ilkesini
	// ("özel olanı say değil, genel olmayanı reddet") ikinci kez ihlal
	// ederdi ve yarın çıkacak bir biçim yine açıkta kalırdı. Kural bu yüzden
	// tek: gömülü adresi çıkar, aynı soruyu ona sor.
	for _, inner := range embeddedIPv4s(addr) {
		if blocked, why := blockedRange(inner); blocked {
			return fmt.Errorf("%w (gömülü IPv4: %s)", ErrBlocked, why)
		}
	}

	return nil
}

// Gömülü IPv4 taşıyan IPv6 önekleri.
var (
	// IPv4-compatible (`::a.b.c.d`, RFC 4291'de kullanımdan kaldırıldı).
	// `::` ve `::1` bu aralıkta ama ikisi de yukarıdaki yüklemlerle zaten
	// engelleniyor.
	ipv4CompatiblePrefix = netip.MustParsePrefix("::/96")
	// 6to4 (RFC 3056): adresin 16-48. bitleri gömülü IPv4.
	sixToFourPrefix = netip.MustParsePrefix("2002::/16")
	// Teredo (RFC 4380): 32-64. bit SUNUCU IPv4'ü, son 32 bit ise
	// XOR 0xffffffff ile gizlenmiş İSTEMCİ IPv4'ü.
	teredoPrefix = netip.MustParsePrefix("2001::/32")
	// NAT64/DNS64 (RFC 6052): son 32 bit gömülü IPv4. Önek ayrıca
	// tümden engelli (bkz. extraBlockedPrefixes) — burada olması, kuralın
	// biçimden bağımsız yazılmasını sağlıyor.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
)

// embeddedIPv4s, adresin içine gömülü IPv4 adreslerini çıkarır.
//
// Boş dilim = gömülü adres yok. Teredo İKİ adres taşıyor (sunucu ve istemci),
// bu yüzden dönüş bir dilim.
func embeddedIPv4s(addr netip.Addr) []netip.Addr {
	if !addr.Is6() {
		return nil
	}

	raw := addr.As16()

	switch {
	case ipv4CompatiblePrefix.Contains(addr), nat64Prefix.Contains(addr):
		return []netip.Addr{ipv4At(raw, 12)}
	case sixToFourPrefix.Contains(addr):
		return []netip.Addr{ipv4At(raw, 2)}
	case teredoPrefix.Contains(addr):
		server := ipv4At(raw, 4)

		// İstemci adresi bit bit tersine çevrilmiş olarak taşınıyor.
		var client [4]byte
		for i := range client {
			client[i] = ^raw[12+i]
		}

		return []netip.Addr{server, netip.AddrFrom4(client)}
	}

	return nil
}

// ipv4At, 16 baytlık gösterimin `offset` konumundaki 4 baytını IPv4 adresi
// olarak okur.
func ipv4At(raw [16]byte, offset int) netip.Addr {
	return netip.AddrFrom4([4]byte(raw[offset : offset+4]))
}

// blockedRange, adresin engellenen bir aralıkta olup olmadığını söyler.
//
// Liste "özel olanı say" değil "genel olmayanı reddet" mantığında: yeni bir
// özel aralık tanımlandığında varsayılan taraf GÜVENLİ olan.
func blockedRange(addr netip.Addr) (bool, string) {
	switch {
	case !addr.IsValid():
		return true, "geçersiz"
	case addr.IsUnspecified():
		// 0.0.0.0 / :: — bazı yığınlarda "bu makine" anlamına geliyor.
		return true, "belirtilmemiş"
	case addr.IsLoopback():
		return true, "loopback"
	case addr.IsPrivate():
		// 10/8, 172.16/12, 192.168/16, fc00::/7
		return true, "özel ağ"
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		// 169.254/16 — BULUT METADATA UCU (169.254.169.254) BURADA.
		return true, "link-local"
	case addr.IsMulticast():
		return true, "multicast"
	case addr.IsInterfaceLocalMulticast():
		return true, "arayüz-yerel multicast"
	}

	// Standart kütüphanenin kapsamadığı, yine de genel İNTERNET olmayan
	// aralıklar. Her biri gerçek bir atlatma yolu:
	for _, blocked := range extraBlockedPrefixes {
		if blocked.prefix.Contains(addr) {
			return true, blocked.why
		}
	}

	return false, ""
}

type blockedPrefix struct {
	prefix netip.Prefix
	why    string
}

// extraBlockedPrefixes, `netip`in hazır yüklemlerinde olmayan aralıklar.
var extraBlockedPrefixes = []blockedPrefix{
	// Carrier-grade NAT: sağlayıcı iç ağı, internet değil.
	{netip.MustParsePrefix("100.64.0.0/10"), "CGNAT"},
	// IETF protokol tahsisi (192.0.0.0/24) ve dokümantasyon blokları.
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protokol"},
	{netip.MustParsePrefix("192.0.2.0/24"), "dokümantasyon"},
	{netip.MustParsePrefix("198.51.100.0/24"), "dokümantasyon"},
	{netip.MustParsePrefix("203.0.113.0/24"), "dokümantasyon"},
	// Ağ karşılaştırma testleri için ayrılmış.
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmark"},
	// 240/4 ve 255.255.255.255 — ayrılmış / yayın.
	{netip.MustParsePrefix("240.0.0.0/4"), "ayrılmış"},
	// IPv6: benzersiz yerel adresler `IsPrivate` ile kapanıyor; kalanlar —
	{netip.MustParsePrefix("::/128"), "belirtilmemiş"},
	{netip.MustParsePrefix("2001:db8::/32"), "dokümantasyon"},
	// NAT64/DNS64 ön eki: içindeki IPv4 adresi ayrıca denetlenmediği sürece
	// iç ağa açılan bir kapı.
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64"},
}
