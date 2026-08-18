package safedial

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

// blocked, politikanın adresi reddedip reddetmediğini söyler.
func blocked(t *testing.T, p Policy, address string) bool {
	t.Helper()

	err := p.Control("tcp", address, nil)
	if err != nil && !errors.Is(err, ErrBlocked) {
		t.Fatalf("%s: beklenmeyen hata tipi: %v", address, err)
	}

	return err != nil
}

// Bu testin listesi bir "iyi olur" listesi değil: her satır gerçek bir SSRF
// hedefi.
func TestBlocksInternalAddresses(t *testing.T) {
	var policy Policy

	addresses := map[string]string{
		"127.0.0.1:80":         "loopback",
		"127.0.0.1:5432":       "yerel postgres",
		"[::1]:80":             "IPv6 loopback",
		"10.0.0.5:80":          "özel ağ",
		"172.16.0.1:80":        "özel ağ",
		"172.31.255.1:80":      "özel ağ (üst sınır)",
		"192.168.1.1:80":       "ev/ofis ağı",
		"169.254.169.254:80":   "BULUT METADATA UCU",
		"169.254.1.1:80":       "link-local",
		"0.0.0.0:80":           "belirtilmemiş",
		"[::]:80":              "IPv6 belirtilmemiş",
		"255.255.255.255:80":   "yayın",
		"224.0.0.1:80":         "multicast",
		"100.64.0.1:80":        "CGNAT",
		"[fc00::1]:80":         "IPv6 benzersiz yerel",
		"[fe80::1]:80":         "IPv6 link-local",
		"198.18.0.1:80":        "benchmark",
		"[64:ff9b::7f00:1]:80": "NAT64 ile sarılmış loopback",
	}

	for address, what := range addresses {
		if !blocked(t, policy, address) {
			t.Errorf("%s (%s) ENGELLENMEDİ", address, what)
		}
	}
}

// IPv4-mapped IPv6 klasik bir atlatma: `::ffff:127.0.0.1` yazımı
// `IsLoopback()` yüklemine takılmaz, önce sarılıştan çıkarılması gerekir.
func TestBlocksIPv4MappedLoopback(t *testing.T) {
	var policy Policy

	for _, address := range []string{
		"[::ffff:127.0.0.1]:80",
		"[::ffff:10.0.0.1]:80",
		"[::ffff:169.254.169.254]:80",
	} {
		if !blocked(t, policy, address) {
			t.Errorf("%s ENGELLENMEDİ — IPv4-mapped yazım korumayı atlatıyor", address)
		}
	}
}

// GÖMÜLÜ IPv4 TAŞIYAN HER YAZIM AYNI KURALDAN GEÇİYOR (7.5).
//
// IPv4-mapped ve NAT64 listeye tek tek yazılmıştı; IPv4-compatible, 6to4 ve
// Teredo yazımları ise açıkta kalmıştı — üçü de bir IPv4 adresini IPv6
// gösteriminin İÇİNE gömüyor, yani aynı sınıf atlatma. Çözüm listeye üç satır
// daha eklemek DEĞİL (dosyanın "özel olanı say değil, genel olmayanı reddet"
// ilkesini ikinci kez ihlal ederdi): gömülü adres ÇIKARILIP aynı kontrol
// yeniden koşuyor, böylece yarın çıkacak dördüncü biçim de kapanıyor.
func TestBlocksIPv4EmbeddedInIPv6Forms(t *testing.T) {
	var policy Policy

	addresses := map[string]string{
		// IPv4-compatible (::a.b.c.d) — kullanımdan kaldırıldı ama yazılabiliyor.
		"[::7f00:1]:80":    "IPv4-compatible loopback (::127.0.0.1)",
		"[::a00:1]:80":     "IPv4-compatible özel ağ (::10.0.0.1)",
		"[::a9fe:a9fe]:80": "IPv4-compatible BULUT METADATA UCU",
		// 6to4 (2002:AABB:CCDD::/48) — ilk 32 bit gömülü IPv4.
		"[2002:7f00:1::]:80":    "6to4 loopback (127.0.0.1)",
		"[2002:a00:1::]:80":     "6to4 özel ağ (10.0.0.1)",
		"[2002:a9fe:a9fe::]:80": "6to4 BULUT METADATA UCU",
		"[2002:c0a8:1::1]:80":   "6to4 ev/ofis ağı (192.168.0.1)",
		// Teredo (2001::/32) — sunucu IPv4'ü ve XOR'lanmış istemci IPv4'ü.
		"[2001:0:7f00:1::]:80":                      "Teredo sunucusu loopback",
		"[2001:0:4136:e378:8000:63bf:3fff:fdd2]:80": "Teredo istemcisi 192.0.2.45",
	}

	for address, what := range addresses {
		if !blocked(t, policy, address) {
			t.Errorf("%s (%s) ENGELLENMEDİ", address, what)
		}
	}
}

// Gömülü IPv4'ü GENEL olan yazımlar geçmeli: kural "IPv6 sarılışı gördüm,
// reddet" değil, "içindeki adrese aynı soruyu sor".
func TestAllowsPublicIPv4EmbeddedInIPv6Forms(t *testing.T) {
	var policy Policy

	for _, address := range []string{
		"[2002:808:808::]:80", // 6to4 → 8.8.8.8
		"[::808:808]:80",      // IPv4-compatible → 8.8.8.8
	} {
		if blocked(t, policy, address) {
			t.Errorf("%s engellendi — gömülü adres genel", address)
		}
	}
}

// Genel adresler geçmeli: koruma çalışmayan bir ürün üretmemeli.
func TestAllowsPublicAddresses(t *testing.T) {
	var policy Policy

	for _, address := range []string{
		"93.184.216.34:443", // example.com
		"1.1.1.1:443",
		"8.8.8.8:53",
		"[2606:4700:4700::1111]:443",
	} {
		if blocked(t, policy, address) {
			t.Errorf("%s engellendi — genel bir adres", address)
		}
	}
}

// TCP dışı ağ tipleri reddediliyor: uygulama yalnızca HTTP konuşuyor.
func TestRejectsNonTCPNetworks(t *testing.T) {
	var policy Policy

	for _, network := range []string{"udp", "unix", "ip4:icmp"} {
		if err := policy.Control(network, "93.184.216.34:443", nil); err == nil {
			t.Errorf("%s ağı kabul edildi", network)
		}
	}
}

// Çözülmemiş bir ad buraya düşerse reddediliyor: izin vermek, korumanın
// tamamen atlanabildiği bir yol bırakırdı.
func TestRejectsUnresolvedHost(t *testing.T) {
	var policy Policy

	if err := policy.Control("tcp", "example.com:443", nil); err == nil {
		t.Error("çözülmemiş ad kabul edildi")
	}
}

// Kapı açıkken loopback geçiyor — yerel geliştirme ve 7.2'nin E2E'si böyle
// çalışıyor.
func TestAllowPrivateOpensTheGate(t *testing.T) {
	policy := Policy{AllowPrivate: true}

	for _, address := range []string{"127.0.0.1:3400", "10.0.0.5:80", "[::1]:80"} {
		if blocked(t, policy, address) {
			t.Errorf("%s engellendi — AllowPrivate açıkken geçmeliydi", address)
		}
	}
}

// Kapı, TCP DIŞI ağları açmıyor: "yerelde her şeye izin" demek değil.
func TestAllowPrivateStillRejectsNonTCP(t *testing.T) {
	policy := Policy{AllowPrivate: true}

	if err := policy.Control("unix", "/tmp/x:0", nil); err == nil {
		t.Error("AllowPrivate açıkken unix soketi kabul edildi")
	}
}

// KORUMA GERÇEK BİR BAĞLANTIDA ÇALIŞIYOR MU: yerelde ayağa kaldırılan bir
// sunucuya `http.Client` ile gidiliyor ve istek HİÇ ULAŞMIYOR.
func TestDialerBlocksRealConnection(t *testing.T) {
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{
		Transport: &http.Transport{DialContext: Policy{}.Dialer(5 * time.Second).DialContext},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(server.URL) //nolint:noctx // test
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("loopback sunucusuna bağlanıldı — koruma çalışmadı")
	}

	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("hata ErrBlocked değil: %v", err)
	}

	if reached {
		t.Error("istek hedefe ULAŞTI — koruma bağlantıdan sonra çalışmış")
	}
}

// Aynı istemci kapı açıkken bağlanabiliyor (testin kendisi de bunu
// kullanıyor: E2E ve yerel geliştirme bu yolda).
func TestDialerAllowsLoopbackWhenPermitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: Policy{AllowPrivate: true}.Dialer(5 * time.Second).DialContext,
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(server.URL) //nolint:noctx // test
	if err != nil {
		t.Fatalf("kapı açıkken bağlanılamadı: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("durum = %d", resp.StatusCode)
	}
}

// YÖNLENDİRME DE DENETLENİYOR: her durak YENİ bir bağlantı, yani yeni bir
// `Control` çağrısı. checker.go'daki "dış görünen bir adres 169.254.169.254'e
// yönlendirebilir" uyarısının cevabı bu — koruma ilk URL'e değil, gerçekten
// gidilen HER adrese uygulanıyor.
//
// Testte iki durak da loopback (httptest başka türlüsünü veremiyor), bu yüzden
// ölçülen şey "ikinci durak reddedildi" değil, "ikinci durak da DENETİMDEN
// GEÇTİ": denetim çalışıyorsa iç bir adres orada reddedilir.
func TestEveryRedirectHopPassesThroughControl(t *testing.T) {
	var checked []string

	policy := Policy{AllowPrivate: true}
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, raw syscall.RawConn) error {
			checked = append(checked, address)

			return policy.Control(network, address, raw)
		},
	}

	last := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer last.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, last.URL, http.StatusFound)
	}))
	defer first.Close()

	client := &http.Client{
		// Bağlantı yeniden kullanımı KAPALI: açık olsaydı ikinci durak aynı
		// bağlantıyı kullanabilir ve testin saydığı şey değişirdi.
		Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(first.URL) //nolint:noctx // test
	if err != nil {
		t.Fatalf("istek başarısız: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if len(checked) != 2 {
		t.Fatalf("denetlenen bağlantı sayısı = %d (%v), beklenen 2 — yönlendirme durağı denetimden geçmemiş",
			len(checked), checked)
	}
}
