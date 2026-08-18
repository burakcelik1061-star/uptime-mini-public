package httpapi

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HIZ SINIRLAMA (7.4).
//
// # Neden gerekli, neden BU uçlar
//
// Sınır, OTURUMSUZ yüzeylere konuyor — kimliği olmayan bir çağıranın
// maliyetli bir işi sınırsız tetikleyebildiği yerler:
//
//   - `/v1/auth/*`: parola deneme (1.1 bilerek "e-posta yok" ile "parola
//     yanlış"ı ayırmıyor, ama sınırsız deneme yine de kaba kuvvet demek) ve
//     sınırsız hesap açma.
//   - `POST /v1/public/status/{slug}/subscribers` (6.4): E-POSTA GÖNDERİMİNİ
//     TETİKLEYEN TEK OTURUMSUZ UÇ. Sınırsız olsaydı bir yabancı, üçüncü
//     kişilere istediği kadar onay e-postası yollatabilirdi.
//   - `POST …/realtime-ticket` ve `GET /v1/ws`, `GET /v1/public/ws`:
//     bağlantı açmak sunucuda bellek ve goroutine tutuyor (6.1'in hub'ı).
//
// Oturumlu ürün uçlarına sınır KONMADI: oradaki çağıranın kimliği ve
// organizasyonu belli, yani kötüye kullanım hesap seviyesinde görünür ve
// yönetilebilir bir sorun. Her uca sınır koymak, normal kullanan bir panonun
// (canlı yayınla birlikte tazelenen ekranlar) kendi kendini kilitlemesi
// riskini getirirdi.
//
// # Neden yeni bir altyapı servisi yok
//
// Sayaç SÜREÇ BELLEĞİNDE. Redis eklemek 0.2'nin "kullanılmayan altyapı
// taşımıyoruz" kararını ve "klonlayan herkes `docker compose up` ile
// çalıştırabilmeli" sözünü (GÜNCEL DURUM §3) bozardı. Bedeli yazılı: çok
// kopyalı bir kurulumda sınır KOPYA BAŞINA uygulanır (üç kopya = üç kat
// sınır). Bu, "sınır yok"tan iyi ve tek kopyalı bu kurulumda tam doğru;
// gerçek bir dağıtık sayaç gerektiğinde yeri bu dosya.
//
// # İstemci kimliği: ham TCP adresi, GÜVENİLEN kaynaklar hariç
//
// `middleware.RealIP` BİLİNÇLİ OLARAK KULLANILMIYOR (0.4'ün kararı):
// `X-Forwarded-For`u DOĞRULAMADAN yazıyor, yani herhangi bir istemci başlığa
// rastgele bir IP koyup HER İSTEKTE YENİ BİR KOVA açabilirdi — sınır sessizce
// hiç uygulanmamış olurdu. O karar duruyor; 0.4 aynı satırda çözümü de
// yazmıştı: "proxy arkasında çalışmak gerekirse orada, GÜVENİLEN proxy
// listesiyle birlikte çözülecek". `TRUSTED_PROXIES` o listedir.
//
// NEDEN GEREKTİ (7.5'in bulgusu): public status sayfası ve abonelik uçları
// 5.1/6.4'ün kararı gereği ziyaretçinin tarayıcısından DEĞİL, Next
// sunucusundan çağrılıyor. Yani api'nin gördüğü `RemoteAddr` tüm
// ziyaretçilerde AYNI — sınır kişi başına değil, SAYFA BAŞINA uygulanıyordu:
// beş isteklik bir kota tüm ziyaretçilerin abonelik/onay/İPTAL yolunu
// on dakikalığına kapatıyordu.
//
// SÖZLEŞME: `X-Forwarded-For` YALNIZCA istek listedeki bir kaynaktan geldiyse
// okunuyor; başka her durumda hiç bakılmıyor (loglanmıyor da — gürültü olur).
// Liste BOŞ başlıyor, yani yapılandırma yapılmadıkça davranış 7.4'tekiyle
// birebir aynı. Listeye bir kaynak yazmak, "bu kaynağın gönderdiği zincire
// güveniyorum" demektir: o kaynağın önünde başlığı temizleyen gerçek bir
// proxy olmalı (Next kendisi istemcinin gönderdiği başlığı olduğu gibi
// geçiriyor).

const (
	// authRateBurst / authRatePeriod: aynı IP'den 10 istek, sonra dakikada
	// 10 yenilenme. Normal bir kullanıcı giriş ekranında bunun yakınına bile
	// gelmiyor; kaba kuvvet ise ilk dakikada duruyor.
	authRateBurst  = 10
	authRatePeriod = time.Minute

	// subscribeRateBurst / subscribeRatePeriod: e-posta gönderimini
	// tetiklediği için daha dar (6.4).
	subscribeRateBurst  = 5
	subscribeRatePeriod = 10 * time.Minute

	// subscriptionLinkRateBurst / subscriptionLinkRatePeriod: ONAY ve İPTAL
	// linkleri — ABONELİK UCUNDAN AYRI KOVA.
	//
	// 7.4 üçünü tek kovada toplamıştı ("ikisi de tahmin edilemez token
	// doğruluyor"). Doğru olan gerekçe, yanlış olan sonuç: iptal linki
	// e-postadaki TEK çıkış yolu ve her zaman çalışmak zorunda — "abone ol"a
	// arka arkaya basan biri başkalarının iptal linkini düşürememeli.
	//
	// Sınırın kendisi duruyor çünkü token doğrulayan bir uç: 256 bitlik bir
	// token kaba kuvvetle bulunamaz ama sınırsız deneme yine de ücretsiz bir
	// tarama yüzeyi olurdu. Kota geniş: meşru trafik bir tık, en fazla birkaç
	// tık (posta istemcisinin link önizlemesi de sayılır).
	subscriptionLinkRateBurst  = 30
	subscriptionLinkRatePeriod = 10 * time.Minute

	// realtimeRateBurst / realtimeRatePeriod: bağlantı açma. Yeniden bağlanma
	// döngüsü (6.1) meşru olarak arka arkaya deneyebiliyor, bu yüzden burst
	// daha geniş.
	realtimeRateBurst  = 30
	realtimeRatePeriod = time.Minute

	// publicReadRateBurst / publicReadRatePeriod: public status sayfasının
	// OKUMA ucu (7.5'te eklendi).
	//
	// NEDEN SINIR, NEDEN ÖNBELLEK DEĞİL: cevabı birkaç saniye önbelleğe almak
	// yükü sınırdan daha iyi düşürürdü ama bu sayfanın TEK İŞİ şu anki durumu
	// söylemek — `public-api.ts` bu kararı `cache: 'no-store'` ile zaten
	// yazmış: "önbelleğe alınmış bir 'tüm sistemler çalışıyor', devam eden bir
	// kesinti sırasında verilebilecek en yanlış cevap". Sunucu tarafında bir
	// TTL, o kararı bir katman aşağıda ve görünmez biçimde geri alırdı.
	// Dürüst kalmasının tek yolu 6.2'nin yayınında önbelleği düşürmek olurdu;
	// o da "neyin değiştiği"nin ikinci bir kopyası demek — 6.2 gövdesiz
	// olayları tam olarak bu kopyayı doğurmamak için seçti.
	//
	// KOTA NEDEN GENİŞ: `TRUSTED_PROXIES` boşken (varsayılan) tüm ziyaretçiler
	// Next sunucusunun tek adresini paylaşıyor, yani kova SAYFA BAŞINA
	// uygulanıyor. Dar bir sınır, çalışan bir status sayfasını kendi
	// ziyaretçilerine kapatırdı. 120/dk kötüye kullanımı iki istek/saniyeye
	// çekiyor ama gerçek trafiğin yakınından geçmiyor.
	publicReadRateBurst  = 120
	publicReadRatePeriod = time.Minute

	// idleBucketTTL, dokunulmayan kovaların temizlenme eşiği. Kova tamamen
	// dolduğunda hiçbir bilgi taşımıyor, yani silmek davranışı değiştirmiyor
	// — yalnızca belleği geri veriyor.
	idleBucketTTL = 10 * time.Minute

	// sweepInterval, temizlik taramasının en sık aralığı.
	sweepInterval = time.Minute
)

// bucket, tek bir istemcinin jeton kovası.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// rateLimiter, anahtar başına jeton kovası tutar.
//
// Jeton kovası seçildi (sabit pencere değil): sabit pencere, pencerenin
// sonunda ve başında arka arkaya iki tam kota harcanmasına izin verir —
// yani sınırın iki katı bir patlama. Kova sürekli dolduğu için böyle bir
// dikiş yeri yok.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	burst float64
	// refill, saniyede eklenen jeton.
	refill float64

	lastSweep time.Time
	// now, testlerin saati sabitleyebilmesi için.
	now func() time.Time
}

// newRateLimiter, `burst` isteği hemen, sonra `per` süresinde `burst` kadar
// yenilenme veren bir sınırlayıcı kurar.
func newRateLimiter(burst int, per time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		burst:   float64(burst),
		refill:  float64(burst) / per.Seconds(),
		now:     time.Now,
	}
}

// allow, bir jeton harcamayı dener. İkinci dönüş, jetonun ne kadar sonra
// hazır olacağı (Retry-After için).
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst}
		l.buckets[key] = b
	}

	// Geçen süre kadar jeton ekle (tavan: burst).
	if !b.lastSeen.IsZero() {
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.lastSeen).Seconds()*l.refill)
	}
	b.lastSeen = now

	if b.tokens < 1 {
		// Bir jetonun dolması için gereken süre.
		wait := time.Duration((1 - b.tokens) / l.refill * float64(time.Second))

		return false, wait
	}

	b.tokens--

	return true, 0
}

// sweep, uzun süredir dokunulmamış kovaları siler. Çağıran kilidi tutuyor.
//
// Bellek sınırı olmadan bir sözlük, her yeni IP için bir satır demek — yani
// sınırlayıcının kendisi bir kaynak tüketimi yüzeyi olurdu.
func (l *rateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now

	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > idleBucketTTL {
			delete(l.buckets, key)
		}
	}
}

// clientIdentifier, isteğin sahibini belirler.
//
// Sıfır değeri (boş liste) 7.4'ün davranışı: yalnızca `RemoteAddr`. Bu bir
// tesadüf değil, VARSAYILANIN GÜVENLİ TARAFI — yapılandırma yapılmadıkça
// hiçbir başlık okunmuyor.
type clientIdentifier struct {
	trusted []netip.Prefix
}

// newClientIdentifier, güvenilen kaynak listesinden bir çözücü kurar.
//
// Girdi tek tek IP'ler ("10.9.0.1") veya bloklar ("172.18.0.0/16") olabilir:
// bir konteyner ağı tek bir adres değil. Çözümlenemeyen girdi SESSİZCE
// atlanıyor değil — yapılandırma doğrulaması `config` tarafında (bkz.
// config.TrustedProxies), buraya yalnızca ayrıştırılmış değerler geliyor.
func newClientIdentifier(sources []string) clientIdentifier {
	prefixes, err := parseTrustedProxies(sources)
	if err != nil {
		// Yapılandırma boot'ta doğrulandığı için buraya düşülmemeli; düşülürse
		// GÜVENLİ tarafa (hiçbir kaynağa güvenme) düşüyoruz.
		return clientIdentifier{}
	}

	return clientIdentifier{trusted: prefixes}
}

// ParseTrustedProxies, `TRUSTED_PROXIES` girdisini doğrular (config kullanıyor).
func ParseTrustedProxies(sources []string) error {
	_, err := parseTrustedProxies(sources)

	return err
}

func parseTrustedProxies(sources []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(sources))

	for _, raw := range sources {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		if strings.Contains(raw, "/") {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("güvenilen kaynak çözümlenemedi (%q): %w", raw, err)
			}

			prefixes = append(prefixes, prefix.Masked())

			continue
		}

		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("güvenilen kaynak çözümlenemedi (%q): %w", raw, err)
		}

		// IPv4-mapped IPv6 sarılışından çıkarılıyor: `::ffff:10.9.0.1` ile
		// `10.9.0.1` aynı kaynak (safedial'ın aynı kararı).
		addr = addr.Unmap()

		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}

	return prefixes, nil
}

// key, isteğin sahibini belirler.
//
// Port atılıyor — aynı istemcinin her isteği farklı bir kaynak portundan gelir
// ve port dahil edilseydi sınır hiç uygulanmazdı.
func (c clientIdentifier) key(r *http.Request) string {
	peer := hostOf(r.RemoteAddr)

	// GÜVENİLMEYEN KAYNAK: başlıklara hiç bakılmıyor.
	if !c.isTrusted(peer) {
		return peer
	}

	// ZİNCİR SAĞDAN SOLA: en sağdaki değer bize EN YAKIN durak. Güvenilen
	// durakları atlayıp ilk güvenilmeyen değeri alıyoruz — soldan okumak,
	// zincirin başına istediğini yazan bir istemciye kimliği seçtirirdi.
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// Bozuk bir değer zincirin geri kalanını da şüpheli yapıyor:
			// bu noktada durup kaynağın kendisine düşüyoruz.
			break
		}

		if candidate := addr.Unmap(); !c.isTrusted(candidate.String()) {
			return candidate.String()
		}
	}

	// Zincir boş ya da tamamı güvenilen duraklardan oluşuyor: uydurma bir
	// değere düşmek yerine kaynağın kendisi anahtar oluyor.
	return peer
}

func (c clientIdentifier) isTrusted(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}

	addr = addr.Unmap()
	for _, prefix := range c.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

func hostOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}

	return host
}

// rateLimit, sınırı uygulayan middleware.
func rateLimit(limiter *rateLimiter, identifier clientIdentifier, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := identifier.key(r)

			allowed, wait := limiter.allow(key)
			if allowed {
				next.ServeHTTP(w, r)

				return
			}

			// Retry-After saniye cinsinden ve en az 1: "0 saniye sonra tekrar
			// dene" bir istemciyi anında yeniden denemeye iter.
			seconds := int(wait.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))

			// Reddedilen istek LOGLANIYOR: sessizce 429 dönmek, gerçek bir
			// saldırıyı da yanlış yapılandırılmış bir istemciyi de görünmez
			// yapardı (2.1'in "reddedilen preflight loglanır" kararının aynısı).
			logger.Warn("hız sınırı aşıldı",
				slog.String("client", key),
				slog.String("path", r.URL.Path),
				slog.String("method", r.Method))

			writeError(w, r, http.StatusTooManyRequests, codeRateLimited,
				"çok fazla istek gönderildi, lütfen biraz bekleyin")
		})
	}
}
