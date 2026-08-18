// Package checker, tek bir monitor'ün TEK BİR kontrolünü çalıştırır:
// hedefe HTTP isteği atar, yanıt süresini ölçer ve sonucu sınıflandırır.
//
// Kapsam sınırı: bu paket ne zamanlama biliyor (3.2) ne durum makinesi (3.3)
// ne de saklama (2.3). Girdisi bir hedef, çıktısı tek bir sonuç — bu yüzden
// hem manuel "şimdi kontrol et" ucu hem de worker aynı kodu çağırabiliyor.
package checker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/safedial"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

const (
	// maxRedirects, izlenecek yönlendirme sayısı (Go'nun varsayılanı 10).
	//
	// Sınırsız bırakılamaz: yönlendirme döngüsü isteği zaman aşımına kadar
	// meşgul ederdi. Kısa tutmak da bilinçli — sağlık kontrolü için beş
	// durak fazlasıyla yeterli.
	//
	// SSRF ve yönlendirme (7.4 — ÇÖZÜLDÜ): yönlendirme, URL doğrulamasını
	// atlamanın klasik yolu — dış görünen bir adres 169.254.169.254'e
	// yönlendirebilir. Denetim `CheckRedirect`e YAZILMADI: her durak yeni bir
	// BAĞLANTI demek ve koruma bağlantı katmanında (`net.Dialer.Control`,
	// bkz. internal/safedial), yani gidilen HER adres kendiliğinden
	// denetleniyor. CheckRedirect'e yazmak, aynı kuralın ikinci bir kopyası
	// ve "yönlendirme dışında bir yol bulunursa açık kalan bir kapı" olurdu.
	maxRedirects = 5

	// maxBodyBytes, okunacak gövde üst sınırı.
	//
	// Gövdenin İÇERİĞİ kullanılmıyor ama tamamen okunuyor: yanıt süresi
	// "sunucu cevabı bitirene kadar" demek — başlığı anında gönderip gövdede
	// takılan bir servis sağlıklı değil. Sınır, sonsuz/devasa bir gövdenin
	// belleği doldurmasını engelliyor (zaman aşımı zaten ayrıca koruyor).
	maxBodyBytes = 64 << 10

	// userAgent, hedef sunucunun loglarında kim olduğumuz görünsün.
	userAgent = "uptime-mini/1.0 (+https://github.com/burakcelik1061-star/uptime-mini)"
)

// ErrCheckNotRun, kontrolün ÇALIŞTIRILAMADIĞINI bildirir: çağıranın context'i
// iptal edildi (süreç kapanıyor, HTTP isteği düştü).
//
// Neden "başarısız kontrol" DEĞİL: kendi zaman aşımımızla hedefin
// cevaplamaması bir ölçümdür, worker'ın kapanması ise ölçüm YOKLUĞUDUR.
// İkisi karıştırılırsa 3.2'de her yeniden başlatma sahte bir kesinti kaydı
// üretir ve 4.1'de gerçek olmayan bir incident açılırdı.
var ErrCheckNotRun = errors.New("kontrol çalıştırılamadı")

// Target, tek bir kontrolün girdisi (monitor kaydının kontrol için gereken
// kısmı).
type Target struct {
	URL     string
	Method  store.MonitorMethod
	Timeout time.Duration
	// Beklenen HTTP status aralığı (dahil).
	ExpectedStatusMin int32
	ExpectedStatusMax int32
	// DegradedAfter sıfırsa "yavaş" sinyali kapalı.
	DegradedAfter time.Duration
}

// TargetOf, monitor kaydından kontrol hedefini çıkarır.
func TargetOf(m store.Monitor) Target {
	target := Target{
		URL:               m.Url,
		Method:            m.Method,
		Timeout:           time.Duration(m.TimeoutMs) * time.Millisecond,
		ExpectedStatusMin: m.ExpectedStatusMin,
		ExpectedStatusMax: m.ExpectedStatusMax,
	}

	if m.DegradedResponseMs != nil {
		target.DegradedAfter = time.Duration(*m.DegradedResponseMs) * time.Millisecond
	}

	return target
}

// Result, tek bir kontrolün sonucu.
//
// Alanlar 0.1'in `check_result` tablosuyla aynı dili konuşuyor; saklama
// 2.3'te.
type Result struct {
	Outcome store.CheckOutcome
	// HTTPStatus sıfırsa cevap hiç alınamadı (bağlantı/isim/TLS hatası).
	HTTPStatus int32
	// ResponseTime, isteğin başlangıcından gövdenin (sınırlı) sonuna kadar.
	ResponseTime time.Duration
	// ErrorType boşsa hata yok. `blocked_url` bu paketten ASLA çıkmaz —
	// o, isteği hiç göndermeyen SSRF korumasının (7.4) sonucu.
	ErrorType store.CheckErrorType
	// ErrorMessage insan içindir; makine tarafı ErrorType'a bakar.
	ErrorMessage string
	CheckedAt    time.Time
}

// Failed, kontrolün başarısız olup olmadığını söyler (degraded başarısız
// DEĞİL — 0.1'in uptime tanımında DEGRADED "up" sayılıyor).
func (r Result) Failed() bool { return r.Outcome == store.CheckOutcomeFailure }

// Checker, kontrolleri çalıştırır. Tek bir örnek uygulama boyunca paylaşılır.
type Checker struct {
	client *http.Client
	// now, testlerin saati sabitleyebilmesi için (üretimde time.Now).
	now func() time.Time
}

// New, paylaşılan istemciyle bir Checker kurar.
//
// http.DefaultClient KULLANILMIYOR: zaman aşımı yok, yönlendirme sınırı 10,
// ve global olduğu için başka bir paketin ayarı bizimkini etkileyebilir.
//
// `policy` SSRF korumasıdır (7.4) ve ZORUNLU bir argüman: varsayılanı olan
// bir seçenek olsaydı, korumayı unutmak mümkün olurdu. Sıfır değeri en
// güvenli olan (iç ağa hiç izin yok).
func New(policy safedial.Policy) *Checker {
	transport := &http.Transport{
		// Bağlantı YENİDEN KULLANILMIYOR. Bedeli her kontrolde yeni bir
		// TCP+TLS el sıkışması; karşılığında iki şey:
		//   1. Ölçümler karşılaştırılabilir kalıyor — havuzdan gelen bir
		//      bağlantı el sıkışmayı atlar ve aynı monitor arka arkaya
		//      "200 ms / 40 ms" gibi anlamsız bir seri üretirdi.
		//   2. Kontrol, saatlerdir açık duran bayat bir bağlantı üzerinden
		//      "başarılı" olamıyor; her kontrol yolun TAMAMINI sınıyor.
		DisableKeepAlives: true,
		// Bağlantı kurma aşamasının kendi sınırı: toplam zaman aşımı zaten
		// context'te, bu yalnızca el sıkışmanın payını sınırlıyor.
		// SSRF KORUMASI BURADA (7.4): `Control`, bağlantı kurulmadan hemen
		// önce ÇÖZÜLMÜŞ IP'yi denetliyor — DNS rebinding penceresi kapalı.
		DialContext:           policy.Dialer(10 * time.Second).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // sınır context'ten geliyor
		// Proxy ortam değişkenleri (HTTP_PROXY) BİLİNÇLİ olarak
		// okunmuyor: kontrol edilen adrese doğrudan gidilmeli, aradaki bir
		// proxy'nin önbelleği "servis ayakta" yanılgısı üretebilir.
		Proxy: nil,
	}

	return &Checker{
		client: &http.Client{
			Transport: transport,
			// Zaman aşımı client'ta DEĞİL context'te: monitor başına farklı
			// ve çağıranın iptali de aynı mekanizmadan geçiyor.
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("en fazla %d yönlendirme izlenir", maxRedirects)
				}

				return nil
			},
		},
		now: time.Now,
	}
}

// Run, hedefi bir kez kontrol eder.
//
// Hata YALNIZCA kontrol hiç çalışamadığında dönüyor (ErrCheckNotRun);
// hedefin cevap vermemesi hata değil, başarısız bir SONUÇ.
func (c *Checker) Run(ctx context.Context, target Target) (Result, error) {
	// Kendi son tarihimiz: hedefin zaman aşımı. Çağıranın context'i daha
	// önce biterse (kapanma sinyali, düşen HTTP isteği) aşağıda ayırt
	// ediliyor.
	checkCtx, cancel := context.WithTimeout(ctx, target.Timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(checkCtx, string(target.Method), target.URL, nil)
	if err != nil {
		// Buraya düşmek yapılandırmanın bozuk olduğu anlamına gelir (URL
		// doğrulaması 2.1'de yapıldı) — yine de sonuç üretiliyor ki monitor
		// sessizce hiç kontrol edilmemiş gibi görünmesin.
		return Result{
			Outcome:      store.CheckOutcomeFailure,
			ErrorType:    store.CheckErrorTypeUnknown,
			ErrorMessage: "istek oluşturulamadı: " + err.Error(),
			CheckedAt:    c.now().UTC(),
		}, nil
	}

	request.Header.Set("User-Agent", userAgent)
	// Sıkıştırılmış gövde yanıt süresine sunucunun sıkıştırma maliyetini de
	// ekler; sağlık kontrolünde gövdenin içeriği zaten kullanılmıyor.
	request.Header.Set("Accept-Encoding", "identity")

	startedAt := c.now()
	response, err := c.client.Do(request) //nolint:bodyclose // aşağıda kapatılıyor
	if err != nil {
		elapsed := c.now().Sub(startedAt)

		// AYRIM: bizim zaman aşımımız mı, çağıranın iptali mi?
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrCheckNotRun, ctx.Err())
		}

		// SSRF KORUMASI REDDETTİ (7.4): bu bir ağ hatası DEĞİL, bizim
		// kararımız — `classify`a düşerse "bağlantı kurulamadı" gibi
		// görünür ve kullanıcı hedefinin neden hiç kontrol edilmediğini
		// anlayamazdı. Hata tipi 2.2'den beri şemada bekliyordu.
		if errors.Is(err, safedial.ErrBlocked) {
			return Result{
				Outcome:      store.CheckOutcomeFailure,
				ResponseTime: elapsed,
				ErrorType:    store.CheckErrorTypeBlockedUrl,
				ErrorMessage: "hedef adres izin verilmeyen bir ağa ait (iç ağ/loopback/link-local)",
				CheckedAt:    c.now().UTC(),
			}, nil
		}

		errorType, message := classify(err)

		return Result{
			Outcome:      store.CheckOutcomeFailure,
			ResponseTime: elapsed,
			ErrorType:    errorType,
			ErrorMessage: message,
			CheckedAt:    c.now().UTC(),
		}, nil
	}
	defer func() { _ = response.Body.Close() }()

	// Gövde okunuyor ama TUTULMUYOR: ölçüm cevabın tamamlanmasını kapsasın.
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxBodyBytes)); err != nil {
		elapsed := c.now().Sub(startedAt)

		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrCheckNotRun, ctx.Err())
		}

		errorType, message := classify(err)

		return Result{
			Outcome: store.CheckOutcomeFailure,
			// Status BİLİNİYOR: başlıklar geldi, gövde yarıda kaldı.
			HTTPStatus:   int32(response.StatusCode), //nolint:gosec // HTTP status 100-599
			ResponseTime: elapsed,
			ErrorType:    errorType,
			ErrorMessage: "cevap gövdesi okunamadı: " + message,
			CheckedAt:    c.now().UTC(),
		}, nil
	}

	elapsed := c.now().Sub(startedAt)

	return c.evaluate(target, int32(response.StatusCode), elapsed), nil //nolint:gosec // HTTP status 100-599
}

// evaluate, cevabı alınmış bir kontrolün sonucunu belirler.
//
// Sıra önemli: önce "beklenen status mu", sonra "yavaş mı". Beklenmeyen bir
// status hızlı geldiyse bu onu başarılı yapmaz.
func (c *Checker) evaluate(target Target, status int32, elapsed time.Duration) Result {
	result := Result{
		HTTPStatus:   status,
		ResponseTime: elapsed,
		CheckedAt:    c.now().UTC(),
	}

	if status < target.ExpectedStatusMin || status > target.ExpectedStatusMax {
		result.Outcome = store.CheckOutcomeFailure
		result.ErrorType = store.CheckErrorTypeUnexpectedStatus
		result.ErrorMessage = fmt.Sprintf("HTTP %d döndü, beklenen %d-%d",
			status, target.ExpectedStatusMin, target.ExpectedStatusMax)

		return result
	}

	// DEGRADED kontrol ANINDA karara bağlanıyor, sonradan eşikten
	// türetilmiyor (0.1 §5): eşik değiştiğinde geçmiş veri anlamını
	// değiştirmesin.
	if target.DegradedAfter > 0 && elapsed >= target.DegradedAfter {
		result.Outcome = store.CheckOutcomeDegraded

		return result
	}

	result.Outcome = store.CheckOutcomeSuccess

	return result
}
