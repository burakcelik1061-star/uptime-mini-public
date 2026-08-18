package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// classify, Go'nun döndürdüğü hatayı şemadaki `check_error_type` değerine
// çevirir.
//
// NEDEN AYRI BİR ENUM (0.1 §5): "hepsi hata" demek, kullanıcıya kesintinin
// SEBEBİNİ söylememek demek. "Alan adı çözülmedi" ile "sertifika geçersiz"
// aynı kutuya girerse ne grafik çıkarılabilir ne de doğru kişiye haber
// verilebilir.
//
// Hata metnine BAKILMIYOR (`strings.Contains(err.Error(), "refused")` gibi):
// metinler Go sürümüne ve işletim sistemine göre değişiyor. Sınıflandırma
// tipler ve sentinel değerler üzerinden — `errors.As` / `errors.Is` sarmalayan
// katmanların (url.Error → net.OpError → os.SyscallError) içine bakıyor.
//
// `blocked_url` BURADA ÜRETİLMEZ: o, isteği hiç göndermeyen SSRF korumasının
// (7.4) sonucu; bu paket isteği çoktan göndermiş oluyor.
func classify(err error) (store.CheckErrorType, string) {
	message := humanMessage(err)

	switch {
	// Kendi son tarihimiz doldu: hedef zamanında cevap vermedi.
	case errors.Is(err, context.DeadlineExceeded):
		return store.CheckErrorTypeTimeout, "hedef zamanında cevap vermedi"

	// İsim çözümlemesi. Zaman aşımına uğrayan bir DNS sorgusu da BURAYA
	// düşüyor (aşağıdaki genel timeout dalına değil): sorun hedefin yavaş
	// olması değil, adının çözülememesi.
	case isDNSError(err):
		return store.CheckErrorTypeDns, message

	// TLS: sertifika doğrulanamadı, alan adı eşleşmedi, süresi doldu ya da
	// karşı taraf hiç TLS konuşmuyor (https ile düz http portu).
	case isTLSError(err):
		return store.CheckErrorTypeTls, message

	// Port kapalı / servis dinlemiyor.
	case isConnectionRefused(err):
		return store.CheckErrorTypeConnectionRefused, "bağlantı reddedildi"

	// Aktarım katmanının kendi zaman aşımları (el sıkışma, bağlantı kurma).
	case isTimeout(err):
		return store.CheckErrorTypeTimeout, message

	default:
		// Bilinmeyen: bağlantının ortadan kopması, yönlendirme sınırının
		// aşılması, protokol hatası… Mesaj insana bırakılıyor.
		return store.CheckErrorTypeUnknown, message
	}
}

func isDNSError(err error) bool {
	var dnsErr *net.DNSError

	return errors.As(err, &dnsErr)
}

// wsaeConnRefused, Windows'un "bağlantı reddedildi" hata numarası (10061).
//
// Neden elle yazılı: Windows'ta `net` paketi soket hatalarını Winsock
// numaralarıyla döndürüyor ve bunlar `syscall.ECONNREFUSED` ile EŞLEŞMİYOR
// (o sabit Windows'ta farklı bir değere karşılık geliyor). Bu satır olmadan
// geliştirme makinesinde her reddedilen bağlantı "unknown" olarak
// sınıflanıyordu — testin yakaladığı gerçek bir hata. Linux'ta bu değere
// sahip bir errno yok, yani karşılaştırma orada zararsız.
const wsaeConnRefused = syscall.Errno(10061)

func isConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaeConnRefused)
}

// isTLSError, TLS/sertifika kaynaklı hataları tanır.
func isTLSError(err error) bool {
	var (
		verification *tls.CertificateVerificationError
		recordHeader tls.RecordHeaderError
		alert        tls.AlertError
		unknownCA    x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalidCert  x509.CertificateInvalidError
	)

	// ErrSchemeMismatch: https ile istendi ama karşı taraf DÜZ HTTP konuştu.
	// net/http bunu TLS katmanına ulaşmadan kendisi tespit ediyor, yani
	// aşağıdaki tls/x509 tipleriyle yakalanmıyor — ama kullanıcının sorunu
	// tam olarak TLS: "bu port TLS konuşmuyor".
	if errors.Is(err, http.ErrSchemeMismatch) {
		return true
	}

	return errors.As(err, &verification) ||
		errors.As(err, &recordHeader) ||
		errors.As(err, &alert) ||
		errors.As(err, &unknownCA) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalidCert)
}

func isTimeout(err error) bool {
	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

// humanMessage, hatayı kullanıcıya gösterilecek hâle getirir.
//
// `*url.Error` sarmalayıcısı ayıklanıyor: metni "Get \"https://…\": …"
// biçiminde ve URL'i kullanıcı zaten monitor kaydında görüyor.
func humanMessage(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}

	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "bilinmeyen hata"
	}

	// Mesaj `check_result.error_message` sütununa yazılacak (2.3) ve
	// arayüzde gösterilecek; uzunluğu sınırlı tutuluyor.
	const maxMessageLength = 300
	if len(message) > maxMessageLength {
		return message[:maxMessageLength] + "…"
	}

	return message
}
