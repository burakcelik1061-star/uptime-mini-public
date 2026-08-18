package monitors

import (
	"errors"
	"strings"
	"testing"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// validSettings, testlerin üzerine tek alan değiştirerek çalıştığı taban.
func validSettings() Settings {
	return Settings{
		Name:                 "API",
		URL:                  "https://example.com/health",
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: 60,
		TimeoutMs:            10000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		FailureThreshold:     DefaultFailureThreshold,
		RecoveryThreshold:    DefaultRecoveryThreshold,
		IsActive:             true,
	}
}

// problemFields, doğrulama hatasındaki alan adlarını döner.
func problemFields(t *testing.T, err error) []string {
	t.Helper()

	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("doğrulama hatası bekleniyordu, gelen: %v", err)
	}

	fields := make([]string, 0, len(v.Problems))
	for _, p := range v.Problems {
		fields = append(fields, p.Field)
	}

	return fields
}

func ptr[T any](value T) *T { return &value }

// "Yavaş" eşiği İSTEĞE BAĞLI: yokken de, zaman aşımına eşitken de geçerli.
func TestValidateDegradedThreshold(t *testing.T) {
	s := validSettings()
	s.DegradedResponseMs = nil
	if err := Validate(s); err != nil {
		t.Errorf("eşiksiz ayar reddedildi: %v", err)
	}

	s.TimeoutMs = 10000
	s.DegradedResponseMs = ptr(int32(10000)) // tam zaman aşımı kadar
	if err := Validate(s); err != nil {
		t.Errorf("sınır değer reddedildi: %v", err)
	}

	s.DegradedResponseMs = ptr(int32(1))
	if err := Validate(s); err != nil {
		t.Errorf("1 ms eşik reddedildi: %v", err)
	}
}

func TestValidateAcceptsValidSettings(t *testing.T) {
	if err := Validate(validSettings()); err != nil {
		t.Fatalf("geçerli ayarlar reddedildi: %v", err)
	}
}

func TestValidateRejectsFields(t *testing.T) {
	tests := []struct {
		name  string
		bozar func(*Settings)
		field string
	}{
		{"boş ad", func(s *Settings) { s.Name = "" }, "name"},
		{"çok uzun ad", func(s *Settings) { s.Name = strings.Repeat("a", MaxNameLength+1) }, "name"},
		{"boş url", func(s *Settings) { s.URL = "" }, "url"},
		{"şemasız url", func(s *Settings) { s.URL = "example.com" }, "url"},
		{"ftp url", func(s *Settings) { s.URL = "ftp://example.com" }, "url"},
		{"javascript url", func(s *Settings) { s.URL = "javascript:alert(1)" }, "url"},
		{"hostsuz url", func(s *Settings) { s.URL = "http:///yol" }, "url"},
		{"kimlik bilgili url", func(s *Settings) { s.URL = "https://ali:gizli@example.com" }, "url"},
		{"çok uzun url", func(s *Settings) { s.URL = "https://example.com/" + strings.Repeat("a", MaxURLLength) }, "url"},
		{"tanınmayan metot", func(s *Settings) { s.Method = store.MonitorMethod("DELETE") }, "method"},
		{"boş metot", func(s *Settings) { s.Method = "" }, "method"},
		{"çok sık aralık", func(s *Settings) { s.CheckIntervalSeconds = 29 }, "checkIntervalSeconds"},
		{"çok seyrek aralık", func(s *Settings) { s.CheckIntervalSeconds = MaxCheckIntervalSeconds + 1 }, "checkIntervalSeconds"},
		{"sıfır aralık", func(s *Settings) { s.CheckIntervalSeconds = 0 }, "checkIntervalSeconds"},
		{"çok kısa timeout", func(s *Settings) { s.TimeoutMs = 999 }, "timeoutMs"},
		{"çok uzun timeout", func(s *Settings) { s.TimeoutMs = MaxTimeoutMs + 1 }, "timeoutMs"},
		{"geçersiz status alt sınırı", func(s *Settings) { s.ExpectedStatusMin = 99 }, "expectedStatusMin"},
		{"geçersiz status üst sınırı", func(s *Settings) { s.ExpectedStatusMax = 600 }, "expectedStatusMax"},
		{"ters status aralığı", func(s *Settings) {
			s.ExpectedStatusMin = 300
			s.ExpectedStatusMax = 200
		}, "expectedStatusMax"},
		{"sıfır degraded eşiği", func(s *Settings) { s.DegradedResponseMs = ptr(int32(0)) }, "degradedResponseMs"},
		{"negatif degraded eşiği", func(s *Settings) { s.DegradedResponseMs = ptr(int32(-1)) }, "degradedResponseMs"},
		{"zaman aşımından büyük degraded eşiği", func(s *Settings) {
			s.TimeoutMs = 5000
			s.DegradedResponseMs = ptr(int32(5001))
		}, "degradedResponseMs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSettings()
			tt.bozar(&s)

			err := Validate(Normalize(s))
			if err == nil {
				t.Fatalf("hata bekleniyordu")
			}

			fields := problemFields(t, err)
			for _, f := range fields {
				if f == tt.field {
					return
				}
			}

			t.Errorf("hatalı alan %q bekleniyordu, gelen: %v", tt.field, fields)
		})
	}
}

// Zaman aşımı kontrol aralığından uzun olamaz: kontroller üst üste binerdi.
func TestValidateRejectsTimeoutLongerThanInterval(t *testing.T) {
	s := validSettings()
	s.CheckIntervalSeconds = 30
	s.TimeoutMs = 60000

	err := Validate(s)
	if err == nil {
		t.Fatal("hata bekleniyordu")
	}

	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("doğrulama hatası bekleniyordu: %v", err)
	}

	if len(v.Problems) != 1 || v.Problems[0].Field != "timeoutMs" {
		t.Fatalf("tek bir timeoutMs hatası bekleniyordu, gelen: %+v", v.Problems)
	}
}

// Sınır değerleri kabul edilmeli (30 sn aralık + tam 30 sn zaman aşımı).
func TestValidateAcceptsBoundaries(t *testing.T) {
	s := validSettings()
	s.CheckIntervalSeconds = MinCheckIntervalSeconds
	s.TimeoutMs = MinCheckIntervalSeconds * 1000

	if err := Validate(s); err != nil {
		t.Fatalf("sınır değerler reddedildi: %v", err)
	}
}

// Aralık aşımı gibi türetilmiş hatalar, alanın kendi sınır hatasını
// ÇOĞALTMAMALI: kullanıcı tek bir hatalı alan için tek mesaj görsün.
func TestValidateDoesNotDuplicateFieldErrors(t *testing.T) {
	s := validSettings()
	s.TimeoutMs = 0

	err := Validate(s)
	fields := problemFields(t, err)

	if len(fields) != 1 {
		t.Fatalf("tek hata bekleniyordu, gelen: %v", fields)
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"  https://example.com/health  ", "https://example.com/health"},
		{"HTTP://Example.COM/Yol", "http://example.com/Yol"},
		{"http://example.com:80/x", "http://example.com/x"},
		{"https://example.com:443/x", "https://example.com/x"},
		{"https://example.com:8443/x", "https://example.com:8443/x"},
		{"https://example.com/x#bolum", "https://example.com/x"},
		{"https://example.com/x?a=1&b=2", "https://example.com/x?a=1&b=2"},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := normalizeURL(tt.raw); got != tt.want {
				t.Errorf("normalizeURL(%q) = %q, beklenen %q", tt.raw, got, tt.want)
			}
		})
	}
}

// Yol büyük/küçük harfi KORUNUYOR: sunucular için /Yol ile /yol farklı
// kaynaklar olabilir. Normalleştirme yalnızca büyük/küçük harfin anlamsız
// olduğu yerlerde (şema, host) uygulanıyor.
func TestNormalizeKeepsPathCase(t *testing.T) {
	if got := normalizeURL("https://EXAMPLE.com/CaseSensitive"); got != "https://example.com/CaseSensitive" {
		t.Errorf("normalizeURL = %q", got)
	}
}

func TestNormalizeTrimsName(t *testing.T) {
	s := Normalize(Settings{Name: "  API  ", URL: "https://example.com"})
	if s.Name != "API" {
		t.Errorf("ad = %q", s.Name)
	}
}

func TestPatchApply(t *testing.T) {
	base := validSettings()
	name := "Yeni ad"
	active := false

	got := Patch{Name: &name, IsActive: &active}.apply(base)

	if got.Name != "Yeni ad" {
		t.Errorf("ad = %q", got.Name)
	}
	if got.IsActive {
		t.Error("isActive false olmalıydı")
	}
	// Gönderilmeyen alanlar korunmalı.
	if got.URL != base.URL || got.TimeoutMs != base.TimeoutMs {
		t.Errorf("dokunulmayan alanlar değişti: %+v", got)
	}
}

func TestPatchIsEmpty(t *testing.T) {
	if !(Patch{}).IsEmpty() {
		t.Error("boş patch IsEmpty olmalı")
	}

	active := true
	if (Patch{IsActive: &active}).IsEmpty() {
		t.Error("dolu patch IsEmpty olmamalı")
	}
}
