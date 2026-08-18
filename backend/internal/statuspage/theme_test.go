package statuspage

import "testing"

// ToneOn saf bir fonksiyon: kontrast kararı gerçek veri kurmadan ölçülüyor.
//
// Ölçülen şey "hangi renk güzel" değil, KULLANICI OKUNMAZ BİR RENK SEÇERSE NE
// OLUYOR sorusunun cevabı: rengin üzerine yazılan tek metnin tonu doğru
// tarafta mı (bkz. theme.go).
func TestToneOn(t *testing.T) {
	tests := map[string]Tone{
		// Açık renkler → koyu metin.
		"#ffffff": ToneDark,
		"#f2b544": ToneDark, // parlak sarı: RGB toplamı orta ama parlaklığı yüksek
		"#35d399": ToneDark,
		"#a8b4c8": ToneDark,
		// 0.2'nin vurgu rengi. Tasarım dokümanı onun üzerine KOYU metin
		// (`--u-text-on-accent`) koyuyor; hesap elle ayarlanmış tasarımla aynı
		// cevabı veriyor.
		"#5b9dff": ToneDark,

		// Koyu renkler → açık metin.
		"#000000": ToneLight,
		"#0b0f16": ToneLight,
		"#7a1020": ToneLight,

		// Biçimsiz değer: doğrulama atlanmış olsa bile karar üretilmeli.
		"kirmizi": ToneLight,
		"":        ToneLight,
	}

	for color, want := range tests {
		if got := ToneOn(color); got != want {
			t.Errorf("ToneOn(%q) = %q, beklenen %q", color, got, want)
		}
	}
}

// Parlak sarı ile orta gri karşılaştırması, "kanal ortalaması > 128" gibi kaba
// bir eşiğin neden yetmediğini gösteriyor: ikisinin RGB toplamı yakın ama
// gözün algıladığı parlaklık çok farklı.
func TestToneOnUsesPerceivedLuminance(t *testing.T) {
	if got := ToneOn("#ffff00"); got != ToneDark {
		t.Errorf("parlak sarı için ton = %q, koyu metin beklenmişti", got)
	}

	if got := ToneOn("#0000ff"); got != ToneLight {
		t.Errorf("saf mavi için ton = %q, açık metin beklenmişti", got)
	}
}

func TestSettingsValidation(t *testing.T) {
	value := func(s string) *string { return &s }

	tests := map[string]settingsValues{
		"biçimsiz renk":     {ThemeColor: value("kirmizi")},
		"kısa hex":          {ThemeColor: value("#fff")},
		"http logo":         {LogoURL: value("http://example.com/logo.png")},
		"javascript logo":   {LogoURL: value("javascript:alert(1)")},
		"parolalı logo":     {LogoURL: value("https://user:pass@example.com/l.png")},
		"çok uzun başlık":   {Title: value(repeatChars(MaxTitleLength + 1))},
		"çok uzun açıklama": {Description: value(repeatChars(MaxDescriptionLength + 1))},
	}

	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			if err := values.normalize().validate(); err == nil {
				t.Error("geçersiz ayar kabul edildi")
			}
		})
	}

	valid := settingsValues{
		Title:       value("  Acme durumu  "),
		Description: value("Servislerimizin güncel durumu."),
		LogoURL:     value("https://cdn.example.com/logo.svg"),
		// Büyük harfli hex kabul ediliyor ama küçültülerek saklanıyor.
		ThemeColor: value("#AABBCC"),
	}

	normalized := valid.normalize()
	if err := normalized.validate(); err != nil {
		t.Fatalf("geçerli ayar reddedildi: %v", err)
	}

	if *normalized.Title != "Acme durumu" {
		t.Errorf("başlık kırpılmadı: %q", *normalized.Title)
	}
	if *normalized.ThemeColor != "#aabbcc" {
		t.Errorf("renk küçültülmedi: %q", *normalized.ThemeColor)
	}
}

// BOŞ DİZE NULL'A DÖNÜYOR: kullanıcı alanı temizlediğinde ortaya çıkan şey
// "boş başlık" değil "başlık yok" — ikisi ayrı kalsaydı public sayfa boş bir
// başlık çizerdi.
func TestSettingsNormalizeClearsBlankValues(t *testing.T) {
	blank := "   "
	values := settingsValues{Title: &blank, Description: &blank, LogoURL: &blank, ThemeColor: &blank}

	normalized := values.normalize()

	if normalized.Title != nil || normalized.Description != nil ||
		normalized.LogoURL != nil || normalized.ThemeColor != nil {
		t.Errorf("boş alanlar null'a çevrilmedi: %+v", normalized)
	}
}

// repeatChars, n karakterlik bir dize üretir (uzunluk sınırı testleri için).
func repeatChars(n int) string {
	buf := make([]rune, n)
	for i := range buf {
		buf[i] = 'a'
	}

	return string(buf)
}
