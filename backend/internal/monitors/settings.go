package monitors

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Monitor yapılandırmasının sınırları.
//
// Sınırlar hem BURADA hem şemadaki CHECK kısıtlarında yazılı (migration
// 00005). İkisi bilinçli olarak aynı: buradaki kopya kullanıcıya alan bazlı
// anlaşılır bir hata verebilmek için, şemadaki kopya ise son hakem — kodda
// bir yol atlanırsa veritabanı yine de bozuk kayıt kabul etmiyor.
const (
	MaxNameLength = 120

	MinCheckIntervalSeconds = 30
	MaxCheckIntervalSeconds = 86400

	MinTimeoutMs = 1000
	MaxTimeoutMs = 60000

	MinHTTPStatus = 100
	MaxHTTPStatus = 599

	// Varsayılanlar: istemci alanı hiç göndermezse bunlar kullanılıyor
	// (şemadaki DEFAULT'larla aynı).
	DefaultMethod               = store.MonitorMethodGET
	DefaultCheckIntervalSeconds = 60
	DefaultTimeoutMs            = 10000
	DefaultExpectedStatusMin    = 200
	DefaultExpectedStatusMax    = 299
)

// Settings, bir monitor'ün KULLANICI TARAFINDAN belirlenen tüm alanları.
//
// `current_status` burada YOK: onu kullanıcı seçmiyor, sistem türetiyor
// (bkz. Service.Create / Service.Update).
type Settings struct {
	Name                 string
	URL                  string
	Method               store.MonitorMethod
	CheckIntervalSeconds int32
	TimeoutMs            int32
	ExpectedStatusMin    int32
	ExpectedStatusMax    int32
	// DegradedResponseMs nil ise "yavaş" sinyali kapalı: kontrol ya başarılı
	// ya başarısız olur, arada bir değer üretilmez (2.2).
	DegradedResponseMs *int32
	// Durum makinesinin eşikleri (3.3): kaç ardışık başarısızlıktan sonra
	// DOWN, kaç ardışık yanıttan sonra tekrar UP. Monitor bazında
	// ayarlanabiliyor çünkü "kesinti" eşiği hedefe göre değişiyor: kritik bir
	// ödeme ucunda 2 hata yeterken, gürültülü bir üçüncü parti serviste 5
	// ardışık hata istemek meşru.
	FailureThreshold  int32
	RecoveryThreshold int32
	IsActive          bool
}

// Problem, tek bir alanın doğrulama sorunu.
//
// httpapi'nin FieldError'ıyla aynı şekle sahip ama ondan bağımsız: bu paket
// HTTP'yi tanımıyor. Çeviriyi handler yapıyor.
type Problem struct {
	Field   string
	Message string
}

type problems struct {
	list []Problem
}

func (p *problems) fail(field, message string) {
	p.list = append(p.list, Problem{Field: field, Message: message})
}

// ValidationError, kabul edilemez alanları taşır.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Field+": "+p.Message)
	}

	return "monitor doğrulanamadı (" + strings.Join(parts, ", ") + ")"
}

// Normalize, saklama biçimine getirir (kırpma, URL normalleştirmesi).
func Normalize(s Settings) Settings {
	s.Name = strings.TrimSpace(s.Name)
	s.URL = normalizeURL(s.URL)

	return s
}

// Validate, alan ve alanlar-arası kuralları denetler.
//
// Doğrulama SERVİSTE (bkz. Service.Create/Update), handler'da değil: monitor
// oluşturmanın ileride başka bir çağıranı olduğunda (seed verisi 7.4, içe
// aktarma) kuralların tekrar yazılması gerekmesin. Handler yalnızca gövdeyi
// çözüp sonucu HTTP'ye çeviriyor.
func Validate(s Settings) error {
	p := &problems{}

	switch {
	case s.Name == "":
		p.fail("name", "zorunlu")
	case utf8.RuneCountInString(s.Name) > MaxNameLength:
		p.fail("name", fmt.Sprintf("en fazla %d karakter olabilir", MaxNameLength))
	}

	validateURL(p, s.URL)

	switch s.Method {
	case store.MonitorMethodGET, store.MonitorMethodHEAD, store.MonitorMethodPOST:
	default:
		// Tanınmayan değer her zaman reddediliyor (boş dize dahil): enum'a
		// ileride yeni bir metot eklendiğinde bu satır derlenmeye devam
		// ederdi ama sessizce geçirmezdi.
		p.fail("method", "GET, HEAD veya POST olmalı")
	}

	if s.CheckIntervalSeconds < MinCheckIntervalSeconds || s.CheckIntervalSeconds > MaxCheckIntervalSeconds {
		p.fail("checkIntervalSeconds", fmt.Sprintf("%d ile %d saniye arasında olmalı",
			MinCheckIntervalSeconds, MaxCheckIntervalSeconds))
	}

	if s.TimeoutMs < MinTimeoutMs || s.TimeoutMs > MaxTimeoutMs {
		p.fail("timeoutMs", fmt.Sprintf("%d ile %d ms arasında olmalı", MinTimeoutMs, MaxTimeoutMs))
	}

	validateStatusRange(p, s)
	validateDegradedThreshold(p, s)
	validateThresholds(p, s)

	// Zaman aşımı kontrol aralığından uzun olamaz: bir monitor'ün kontrolleri
	// üst üste binerdi. Yalnızca ikisi de kendi aralığındayken bakılıyor —
	// aksi hâlde tek bir hatalı alan iki hata mesajı üretirdi.
	if s.TimeoutMs >= MinTimeoutMs && s.TimeoutMs <= MaxTimeoutMs &&
		s.CheckIntervalSeconds >= MinCheckIntervalSeconds && s.CheckIntervalSeconds <= MaxCheckIntervalSeconds &&
		int64(s.TimeoutMs) > int64(s.CheckIntervalSeconds)*1000 {
		p.fail("timeoutMs", "kontrol aralığından uzun olamaz")
	}

	if len(p.list) > 0 {
		return &ValidationError{Problems: p.list}
	}

	return nil
}

// validateDegradedThreshold, "yavaş" eşiğini denetler.
//
// Eşik ZAMAN AŞIMINDAN büyük olamaz: daha yavaş bir cevap zaten timeout'a
// takılır, yani ulaşılamayan bir eşik sessiz bir yapılandırma hatası olurdu.
// Zaman aşımı kendi sınırının dışındaysa burada hiç bakılmıyor — tek hatalı
// alan iki mesaj üretmesin.
func validateDegradedThreshold(p *problems, s Settings) {
	if s.DegradedResponseMs == nil {
		return
	}

	threshold := *s.DegradedResponseMs
	if threshold < 1 {
		p.fail("degradedResponseMs", "en az 1 ms olmalı")
		return
	}

	if s.TimeoutMs >= MinTimeoutMs && s.TimeoutMs <= MaxTimeoutMs && threshold > s.TimeoutMs {
		p.fail("degradedResponseMs", "zaman aşımından büyük olamaz")
	}
}

// validateThresholds, durum makinesinin eşiklerini denetler (3.3).
//
// Sınırlar şemadaki CHECK ile aynı; buradaki kopya kullanıcıya alan bazlı
// hata verebilmek için (aynı disiplin: uygulama açıklar, veritabanı son
// hakem).
func validateThresholds(p *problems, s Settings) {
	if s.FailureThreshold < MinThreshold || s.FailureThreshold > MaxThreshold {
		p.fail("failureThreshold", fmt.Sprintf("%d ile %d arasında olmalı", MinThreshold, MaxThreshold))
	}

	if s.RecoveryThreshold < MinThreshold || s.RecoveryThreshold > MaxThreshold {
		p.fail("recoveryThreshold", fmt.Sprintf("%d ile %d arasında olmalı", MinThreshold, MaxThreshold))
	}
}

func validateStatusRange(p *problems, s Settings) {
	minOK := s.ExpectedStatusMin >= MinHTTPStatus && s.ExpectedStatusMin <= MaxHTTPStatus
	maxOK := s.ExpectedStatusMax >= MinHTTPStatus && s.ExpectedStatusMax <= MaxHTTPStatus

	if !minOK {
		p.fail("expectedStatusMin", fmt.Sprintf("%d ile %d arasında olmalı", MinHTTPStatus, MaxHTTPStatus))
	}

	if !maxOK {
		p.fail("expectedStatusMax", fmt.Sprintf("%d ile %d arasında olmalı", MinHTTPStatus, MaxHTTPStatus))
	}

	if minOK && maxOK && s.ExpectedStatusMin > s.ExpectedStatusMax {
		p.fail("expectedStatusMax", "alt sınırdan küçük olamaz")
	}
}
