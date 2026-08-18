package statuspage

// Status page ÖZELLEŞTİRMESİ (Faz 5.3): başlık, açıklama, logo, tema rengi.
//
// Dört alanın da ortak özelliği NULL OLABİLMESİ ("başlık yok" = organizasyonun
// adı, "renk yok" = varsayılan vurgu). Bu yüzden kısmi güncellemede üç durum
// gerekiyor — gönderilmedi · null gönderildi · değer gönderildi — ve desen
// 2.2/2.1'den beri aynı: işaretçi + ayrı bir `Set…` bayrağı.

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxTitleLength / MaxDescriptionLength / MaxLogoURLLength, şemadaki
	// CHECK kısıtlarıyla aynı sınırlar.
	MaxTitleLength       = 120
	MaxDescriptionLength = 500
	MaxLogoURLLength     = 2048
)

// themeColorPattern, kabul edilen tema rengi biçimi (şemadaki CHECK ile aynı).
//
// SERBEST METİN DEĞİL: bu değer arayüzde bir CSS özelliğine yazılıyor.
// "red", "rgb(0 0 0)" ya da `url(javascript:…)` gibi bir girdinin oraya
// ulaşabilmesi, denetlenmemiş bir metnin stile karışması demek olurdu. Altı
// haneli hex, hem tarayıcıda hem şemada tek bir doğrulanabilir biçim.
var themeColorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// SettingsPatch, kısmi güncellemenin girdisi.
//
// `IsPublished` dışındaki alanlarda nil "dokunma" DEĞİL, "null yaz" olabilir;
// ayrımı `Set…` bayrakları taşıyor (monitors.Patch'teki
// `SetDegradedResponseMs` ile aynı gerekçe).
type SettingsPatch struct {
	IsPublished *bool

	Title    *string
	SetTitle bool

	Description    *string
	SetDescription bool

	LogoURL    *string
	SetLogoURL bool

	ThemeColor    *string
	SetThemeColor bool
}

// IsEmpty, hiçbir alanın gönderilmediğini söyler.
//
// Boş bir PATCH sessizce "hiçbir şey yapma" olmamalı: istemci ne istediğini
// söylemeli (5.1'den beri geçerli kural).
func (p SettingsPatch) IsEmpty() bool {
	return p.IsPublished == nil &&
		!p.SetTitle && !p.SetDescription && !p.SetLogoURL && !p.SetThemeColor
}

// apply, gönderilen alanları mevcut kaydın üzerine bindirir.
//
// Birleştirme UYGULAMADA, SQL'de `COALESCE` ile değil (2.1'in kararı): dört
// alanın dördü de NULL olabildiği için `COALESCE` "alanı temizle" ile "alana
// dokunma"yı ayırt edemezdi.
func (p SettingsPatch) apply(current settingsValues) settingsValues {
	if p.IsPublished != nil {
		current.IsPublished = *p.IsPublished
	}

	if p.SetTitle {
		current.Title = p.Title
	}

	if p.SetDescription {
		current.Description = p.Description
	}

	if p.SetLogoURL {
		current.LogoURL = p.LogoURL
	}

	if p.SetThemeColor {
		current.ThemeColor = p.ThemeColor
	}

	return current
}

// settingsValues, sayfanın yazılabilir alanlarının tamamı.
type settingsValues struct {
	IsPublished bool
	Title       *string
	Description *string
	LogoURL     *string
	ThemeColor  *string
}

// normalize, girdiyi saklama biçimine getirir.
//
// BOŞ DİZE NULL'A ÇEVRİLİYOR: kullanıcı başlık alanını temizlediğinde ortaya
// çıkan şey "boş başlık" değil "başlık yok" — ve ikisi ayrı kalsaydı public
// sayfa boş bir `<h1>` çizerdi. Aynı sebeple tema rengi küçültülüyor: "#AABBCC"
// ile "#aabbcc" aynı renk, iki farklı satır değil.
func (v settingsValues) normalize() settingsValues {
	v.Title = normalizeOptional(v.Title, strings.TrimSpace)
	v.Description = normalizeOptional(v.Description, strings.TrimSpace)
	v.LogoURL = normalizeOptional(v.LogoURL, strings.TrimSpace)
	v.ThemeColor = normalizeOptional(v.ThemeColor, func(raw string) string {
		return strings.ToLower(strings.TrimSpace(raw))
	})

	return v
}

func normalizeOptional(value *string, clean func(string) string) *string {
	if value == nil {
		return nil
	}

	cleaned := clean(*value)
	if cleaned == "" {
		return nil
	}

	return &cleaned
}

// validate, sayfa ayarlarını denetler.
func (v settingsValues) validate() error {
	var problems []Problem

	if v.Title != nil && utf8.RuneCountInString(*v.Title) > MaxTitleLength {
		problems = append(problems, Problem{
			Field:   "title",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxTitleLength),
		})
	}

	if v.Description != nil && utf8.RuneCountInString(*v.Description) > MaxDescriptionLength {
		problems = append(problems, Problem{
			Field:   "description",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxDescriptionLength),
		})
	}

	problems = append(problems, validateLogoURL(v.LogoURL)...)

	if v.ThemeColor != nil && !themeColorPattern.MatchString(*v.ThemeColor) {
		problems = append(problems, Problem{
			Field:   "themeColor",
			Message: "#rrggbb biçiminde bir renk olmalı",
		})
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

// validateLogoURL, logo adresini denetler.
//
// YALNIZCA https: http bir logo, https ile açılmış bir sayfada karışık içerik
// uyarısı üretir ve tarayıcıların çoğu görseli hiç yüklemez — yani "logom
// görünmüyor" diye geri gelen bir hata. `data:` ve `javascript:` şemaları da
// böylece kapanıyor.
//
// ADRESİN GERÇEKTEN BİR GÖRSEL OLDUĞU DOĞRULANMIYOR (bilinçli): doğrulamak
// için sunucunun o adrese istek atması gerekirdi ve bu, 7.4'ün SSRF konusu.
// En kötü durumda logo yüklenmez; sayfa çalışmaya devam eder.
func validateLogoURL(raw *string) []Problem {
	if raw == nil {
		return nil
	}

	problem := []Problem{{Field: "logoUrl", Message: "https ile başlayan geçerli bir adres olmalı"}}

	if len(*raw) > MaxLogoURLLength {
		return []Problem{{
			Field:   "logoUrl",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxLogoURLLength),
		}}
	}

	parsed, err := url.Parse(*raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return problem
	}

	// Kimlik bilgisi taşıyan adres reddediliyor (2.1'in URL doğrulamasıyla
	// aynı kural): parola bir status page ayarında saklanmamalı.
	if parsed.User != nil {
		return problem
	}

	return nil
}
