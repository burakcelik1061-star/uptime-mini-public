package statuspage

// TEMA RENGİNİN KONTRAST RİSKİ (Faz 5.3).
//
// Kullanıcı istediği rengi seçebiliyor ve okunmaz bir renk seçmesi (arka planla
// aynı koyulukta bir lacivert, ya da göz alıcı bir sarı) engellenemiyor —
// engellemeye çalışmak "şu renkleri seçemezsin" diyen bir ürün demek olurdu.
// Risk üç kararla ele alınıyor:
//
//  1. RENK HİÇBİR ZAMAN BİLGİ TAŞIMIYOR. Durum (çalışıyor/kesinti/bakım/veri
//     yok) her yerde 0.2'nin SABİT durum token'larıyla ve ayrıca ŞEKİL +
//     METİNLE anlatılıyor; kullanıcının seçtiği renk yalnızca kimlik
//     (logo yeri, ince vurgu çizgisi, grup başlığı kenarı). Okunmaz bir seçim
//     sayfayı çirkinleştirebilir, YANLIŞ ya da anlaşılmaz yapamaz.
//
//  2. RENK TEK BİR DEĞİŞKENE BAĞLANIYOR (`--u-brand`), bileşenlere ham değer
//     olarak serpilmiyor. Nerede kullanıldığı tek bir CSS dosyasından
//     okunabiliyor ve varsayılanı `var(--u-accent)`.
//
//  3. RENGİN ÜZERİNE YAZILAN TEK METİN için ton SUNUCUDA seçiliyor (aşağıdaki
//     `Tone`). Arayüz "koyu mu açık mı" hesabını yeniden yapmıyor, gelen tona
//     göre mevcut metin token'ını seçiyor — yani ham renk değeri yine
//     arayüzde yazılmıyor.

import (
	"math"
	"strconv"
	"strings"
)

// Tone, tema renginin ÜZERİNE yazılacak metnin tonu.
//
// Sözleşmede renk değil TON dönüyor: istemciye "#08111f yaz" demek, arayüzde
// ham değer yasağını (0.2) sunucu üzerinden delmek olurdu. İstemci tona bakıp
// kendi token'ını seçiyor.
type Tone string

const (
	// ToneDark — tema rengi AÇIK, üzerine koyu metin gelmeli.
	ToneDark Tone = "dark"
	// ToneLight — tema rengi KOYU, üzerine açık metin gelmeli.
	ToneLight Tone = "light"
)

// contrastThreshold, koyu/açık metin sınırı.
//
// 0.179, WCAG'nin bağıl parlaklık formülünde siyah ve beyaz metnin kontrast
// oranlarının eşitlendiği nokta: bu değerin üzerindeki her renk koyu metinle,
// altındaki her renk açık metinle daha yüksek kontrast veriyor. "Renk açık mı"
// sorusunu göz kararı bir eşikle (ör. ortalama > 128) cevaplamak, sarı gibi
// parlak ama düşük RGB toplamlı renklerde yanlış tarafı seçerdi.
const contrastThreshold = 0.179

// ToneOn, verilen tema rengi için metin tonunu döndürür.
//
// SAF FONKSİYON (3.3'ün `NextState`, 5.1'in `Summarize` disiplini): girdisi bir
// hex dizesi, çıktısı bir ton. Renk çözümlenemezse (biçim doğrulaması
// atlanmışsa) varsayılan tema rengi kadar koyu kabul ediliyor — bir hata
// döndürmek, bu kararın hiçbir çağıranı için anlamlı bir seçenek değil.
func ToneOn(hexColor string) Tone {
	red, green, blue, ok := parseHex(hexColor)
	if !ok {
		return ToneLight
	}

	// WCAG 2.x bağıl parlaklık (relative luminance).
	luminance := 0.2126*channelLuminance(red) +
		0.7152*channelLuminance(green) +
		0.0722*channelLuminance(blue)

	if luminance > contrastThreshold {
		return ToneDark
	}

	return ToneLight
}

// channelLuminance, sRGB kanalını doğrusal ışık değerine çevirir.
func channelLuminance(value float64) float64 {
	channel := value / 255

	if channel <= 0.04045 {
		return channel / 12.92
	}

	// Gamma düzeltmesinin tersi. Yaklaşık bir üs (2.2) yerine WCAG'nin tam
	// formülü: eşiğin yakınındaki renklerde iki hesap farklı ton seçebiliyor.
	return math.Pow((channel+0.055)/1.055, 2.4)
}

// parseHex, `#rrggbb` dizesini kanallara ayırır.
func parseHex(hexColor string) (red, green, blue float64, ok bool) {
	if !themeColorPattern.MatchString(strings.ToLower(strings.TrimSpace(hexColor))) {
		return 0, 0, 0, false
	}

	value, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(hexColor), "#"), 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}

	return float64(value >> 16 & 0xff), float64(value >> 8 & 0xff), float64(value & 0xff), true
}
