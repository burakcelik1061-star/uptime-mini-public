package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Sınıflandırma SAF bir fonksiyon: gerçek hata DEĞERLERİ verilerek ağ
// olmadan ölçülüyor. DNS hatası gibi dalları uçtan uca denemek gerçek bir
// isim sorgusu (yani ağ bağımlılığı) isterdi.
//
// Hatalar üretimdeki gibi SARMALANMIŞ hâlde veriliyor (url.Error →
// net.OpError → os.SyscallError): sınıflandırmanın sarmalayıcıların içine
// bakabildiği de böylece ölçülüyor.

// wrapped, net/http'nin döndürdüğü sarmalama zincirini taklit eder.
func wrapped(inner error) error {
	return &url.Error{
		Op:  "Get",
		URL: "https://example.com/health",
		Err: inner,
	}
}

func dialError(inner error) error {
	return wrapped(&net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080},
		Err:  inner,
	})
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want store.CheckErrorType
	}{
		{
			name: "kendi zaman aşımımız",
			err:  wrapped(context.DeadlineExceeded),
			want: store.CheckErrorTypeTimeout,
		},
		{
			name: "isim çözülemedi",
			err:  dialError(&net.DNSError{Err: "no such host", Name: "yok.example", IsNotFound: true}),
			want: store.CheckErrorTypeDns,
		},
		{
			// DNS'in kendi zaman aşımı: sorun hedefin yavaşlığı değil,
			// adının çözülememesi.
			name: "dns zaman aşımı yine dns",
			err:  dialError(&net.DNSError{Err: "i/o timeout", Name: "yavas.example", IsTimeout: true}),
			want: store.CheckErrorTypeDns,
		},
		{
			name: "bağlantı reddedildi (unix)",
			err:  dialError(os.NewSyscallError("connect", syscall.ECONNREFUSED)),
			want: store.CheckErrorTypeConnectionRefused,
		},
		{
			name: "bağlantı reddedildi (windows/winsock)",
			err:  dialError(os.NewSyscallError("connectex", wsaeConnRefused)),
			want: store.CheckErrorTypeConnectionRefused,
		},
		{
			name: "sertifika doğrulanamadı",
			err:  wrapped(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}),
			want: store.CheckErrorTypeTls,
		},
		{
			name: "sertifika alan adıyla eşleşmiyor",
			// Certificate alanı dolduruluyor: x509.HostnameError.Error()
			// sertifikayı okuyor (gerçek hatalarda her zaman dolu).
			err:  wrapped(x509.HostnameError{Host: "example.com", Certificate: &x509.Certificate{}}),
			want: store.CheckErrorTypeTls,
		},
		{
			name: "sertifikanın süresi dolmuş",
			err:  wrapped(x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}),
			want: store.CheckErrorTypeTls,
		},
		{
			name: "karşı taraf tls konuşmuyor",
			err:  wrapped(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}),
			want: store.CheckErrorTypeTls,
		},
		{
			name: "tls uyarısı",
			err:  wrapped(tls.AlertError(80)),
			want: store.CheckErrorTypeTls,
		},
		{
			name: "https istendi, düz http cevabı geldi",
			err:  wrapped(http.ErrSchemeMismatch),
			want: store.CheckErrorTypeTls,
		},
		{
			name: "el sıkışma zaman aşımı",
			err:  dialError(&timeoutError{}),
			want: store.CheckErrorTypeTimeout,
		},
		{
			name: "bağlantı ortadan koptu",
			err:  wrapped(errors.New("EOF")),
			want: store.CheckErrorTypeUnknown,
		},
		{
			name: "yönlendirme sınırı",
			err:  wrapped(fmt.Errorf("en fazla %d yönlendirme izlenir", maxRedirects)),
			want: store.CheckErrorTypeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, message := classify(tt.err)
			if got != tt.want {
				t.Errorf("tip = %q, beklenen %q (mesaj: %s)", got, tt.want, message)
			}
			if message == "" {
				t.Error("mesaj boş")
			}
		})
	}
}

// timeoutError, net.Error arayüzünü karşılayan zaman aşımı hatası.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }
func (timeoutError) Temporary() bool {
	return true
}

// `blocked_url` bu paketten ASLA çıkmamalı: o, isteği hiç göndermeyen SSRF
// korumasının (7.4) sonucu.
func TestClassifyNeverProducesBlockedURL(t *testing.T) {
	errs := []error{
		wrapped(context.DeadlineExceeded),
		dialError(&net.DNSError{Err: "no such host"}),
		dialError(os.NewSyscallError("connect", syscall.ECONNREFUSED)),
		wrapped(x509.HostnameError{Host: "example.com", Certificate: &x509.Certificate{}}),
		wrapped(errors.New("bilinmeyen")),
	}

	for _, err := range errs {
		if got, _ := classify(err); got == store.CheckErrorTypeBlockedUrl {
			t.Errorf("%v → blocked_url", err)
		}
	}
}

// Kullanıcıya gösterilen mesaj, isteğin tekrarlanan "Get https://…" önekini
// taşımamalı ve sınırsız uzayamamalı.
func TestHumanMessage(t *testing.T) {
	message := humanMessage(wrapped(errors.New("bir şeyler ters gitti")))
	if strings.Contains(message, "https://example.com") {
		t.Errorf("url mesajda tekrarlanıyor: %q", message)
	}

	long := humanMessage(errors.New(strings.Repeat("x", 500)))
	if len([]rune(long)) > 320 {
		t.Errorf("mesaj kısaltılmamış: %d karakter", len([]rune(long)))
	}
}
