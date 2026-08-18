package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORS (Faz 2.1) — tarayıcı arayüzü (:3100) ile api (:3400) FARKLI ORIGIN'ler
// olduğu için gerekli. `WEB_ORIGINS` 0.3'ten beri yapılandırmada duruyordu;
// ilk tüketicisi bu adım.
//
// Neden hazır bir kütüphane (go-chi/cors) yok: burada ihtiyaç duyulan kural
// seti kısa ve kritik olan tarafı YANLIŞ YAPMAMAK. Kütüphanenin esnek
// seçenekleri (AllowedOriginFunc, wildcard desenleri, AllowAll) bu projede
// kullanılmayacak ve her biri yanlış yapılandırılabilecek bir düğme.
//
// Sözleşme:
//   - Origin listesi TAM EŞLEŞME ile denetleniyor; joker karakter yok.
//   - `Access-Control-Allow-Origin: *` HİÇBİR ZAMAN yazılmıyor. Kimlik
//     bilgisi taşıyan isteklerde tarayıcı zaten `*` kabul etmiyor ve
//     yazılabilseydi refresh cookie'si herhangi bir siteye açılırdı.
//   - İzin verilmeyen origin'e CORS başlığı YAZILMIYOR: tarayıcı cevabı
//     JavaScript'e vermiyor.
const (
	// Liste TAM OLARAK router'ın kullandığı metotlar olmalı. `PUT` 5.1'de
	// eklendi (status page'in servis seçimi tümden değiştiriliyor) ve eksikliği
	// TARAYICIDA yakalandı: preflight cevabı `PUT`u saymadığı için istek hiç
	// gönderilmiyor, arayüzde ise ayırt edilemez bir "Sunucuya ulaşılamadı"
	// hatası görünüyordu — sunucu logunda da bir iz yok, çünkü asıl istek
	// gerçekten hiç ulaşmıyor. Yeni bir metot kullanan uç eklendiğinde burası
	// da güncellenmeli.
	corsAllowedMethods = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS"

	// İzin verilen istek başlıkları SABİT: isteğin
	// `Access-Control-Request-Headers` değerini aynen yansıtmak, herhangi bir
	// başlığı otomatik onaylamak demek olurdu.
	corsAllowedHeaders = "Authorization, Content-Type"

	// Preflight cevabının tarayıcıda önbelleklenme süresi. Her istekten önce
	// ikinci bir tur atmamak için; çok uzun tutmak yapılandırma değişikliğini
	// geç fark ettirir.
	corsMaxAge = 10 * time.Minute
)

// corsMiddleware, çapraz origin isteklerini yapılandırmadaki listeye göre
// karşılar.
func corsMiddleware(allowedOrigins []string, logger *slog.Logger) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		// Yapılandırmada sondaki eğik çizgi kolay bir yazım hatası; Origin
		// başlığında o çizgi hiçbir zaman olmuyor.
		if normalized := strings.TrimRight(strings.TrimSpace(origin), "/"); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}

	if len(allowed) == 0 {
		// Sessiz kalmamalı: bu yapılandırmayla arayüz api'yi çağıramaz ve
		// tarayıcı konsolundaki hata sebebi göstermez.
		logger.Warn("WEB_ORIGINS boş — tarayıcıdan gelen çapraz origin istekleri reddedilecek")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Origin yok: tarayıcı dışı istemci (curl, worker, sağlık
				// kontrolü) veya aynı origin. CORS'un konusu değil.
				next.ServeHTTP(w, r)
				return
			}

			// Vary: aynı yolun cevabı Origin'e göre DEĞİŞİYOR. Bu başlık
			// olmadan bir ara önbellek, bir origin için üretilmiş cevabı
			// başkasına servis edebilirdi.
			w.Header().Add("Vary", "Origin")

			_, ok := allowed[strings.TrimRight(origin, "/")]

			// Preflight: tarayıcı asıl isteği göndermeden önce izin soruyor.
			// Ayırt edici işaret Access-Control-Request-Method başlığı — bu
			// olmadan gelen bir OPTIONS normal bir istektir.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")

				if !ok {
					// 403 + log: preflight'ın cevabını JavaScript zaten
					// okuyamıyor, ama yanlış yapılandırılmış bir WEB_ORIGINS
					// burada sunucu logundan görülebilsin. Sessizce 204
					// dönmek, sorunu tarayıcı konsolundaki "CORS hatası"
					// mesajına havale ederdi.
					logger.Warn("izin verilmeyen origin'den preflight",
						slog.String("origin", origin),
						slog.String("path", r.URL.Path))
					writeError(w, r, http.StatusForbidden, codeForbidden, "bu origin'e izin verilmiyor")

					return
				}

				writeCORSHeaders(w, origin)
				w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(corsMaxAge.Seconds())))
				w.WriteHeader(http.StatusNoContent)

				return
			}

			if ok {
				writeCORSHeaders(w, origin)
			}

			// İzin verilmeyen origin'in isteği yine de İŞLENİYOR: tarayıcı
			// basit istekleri (GET/POST) zaten gönderir, CORS onları
			// engellemez — CEVABIN OKUNMASINI engeller. Sunucu tarafında
			// asıl koruma kimlik doğrulaması: access token'ı tarayıcı
			// kendiliğinden eklemiyor, refresh cookie'si ise SameSite=Strict.
			next.ServeHTTP(w, r)
		})
	}
}

func writeCORSHeaders(w http.ResponseWriter, origin string) {
	// Origin başlığı OLDUĞU GİBİ yansıtılıyor (yapılandırmadaki hâli değil):
	// listede olduğu zaten doğrulandı ve tarayıcı birebir eşleşme bekliyor.
	w.Header().Set("Access-Control-Allow-Origin", origin)
	// Kimlik bilgisi (refresh cookie'si) taşıyan istekler için şart.
	w.Header().Set("Access-Control-Allow-Credentials", "true")
}
