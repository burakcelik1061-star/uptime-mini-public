package orgs

import (
	"crypto/rand"
	"encoding/base32"
	"strconv"
	"strings"
)

const (
	// maxSlugLength, üretilen slug'ın üst sınırı. Veritabanı kısıtı 63
	// (alt alan adı etiketi sınırı, Faz 5.4); ek/sayı takısı için pay
	// bırakılıyor.
	maxSlugLength = 40

	// fallbackSlug, ad tamamen slug'a çevrilemediğinde kullanılır
	// (ör. "日本語" veya "!!!"). Boş slug veritabanı kısıtına takılırdı.
	fallbackSlug = "org"
)

// transliterations, ASCII dışı harflerin karşılıkları.
//
// Türkçe harfler burada ÖZELLİKLE var: "Şirket Adı" → "sirket-adi" olmalı,
// "-irket-ad-" değil. Go'nun strings.ToLower'ı 'İ' için 'i' + birleşik
// nokta üretiyor; harfleri küçültmeden ÖNCE çevirmek bu sürprizi de
// ortadan kaldırıyor.
var transliterations = map[rune]string{
	'ç': "c", 'Ç': "c",
	'ğ': "g", 'Ğ': "g",
	'ı': "i", 'I': "i", 'İ': "i",
	'ö': "o", 'Ö': "o",
	'ş': "s", 'Ş': "s",
	'ü': "u", 'Ü': "u",
	'â': "a", 'à': "a", 'á': "a", 'ä': "a", 'å': "a", 'Â': "a", 'À': "a", 'Á': "a", 'Ä': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e", 'É': "e", 'È': "e", 'Ê': "e", 'Ë': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i", 'Í': "i", 'Ì': "i", 'Î': "i", 'Ï': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'õ': "o", 'Ó': "o", 'Ò': "o", 'Ô': "o", 'Õ': "o",
	'ú': "u", 'ù': "u", 'û': "u", 'Ú': "u", 'Ù': "u", 'Û': "u",
	'ñ': "n", 'Ñ': "n",
	'ß': "ss",
}

// slugify, organizasyon adından URL'de kullanılabilir bir slug üretir.
//
// Sonuç her zaman veritabanındaki kısıtı sağlar: yalnızca küçük harf/rakam,
// aralarında tek tire, baş/sonda tire yok, boş değil.
func slugify(name string) string {
	var builder strings.Builder

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + ('a' - 'A'))
		default:
			if replacement, ok := transliterations[r]; ok {
				builder.WriteString(replacement)
				continue
			}
			// Boşluk, noktalama, emoji, çevrilemeyen harfler: hepsi ayraç.
			builder.WriteByte('-')
		}
	}

	slug := collapseHyphens(builder.String())

	if len(slug) > maxSlugLength {
		// Kesme yalnızca ASCII üzerinde yapılıyor (yukarıdaki döngüden
		// sonra dize zaten ASCII), bu yüzden byte sınırı güvenli.
		slug = strings.Trim(slug[:maxSlugLength], "-")
	}

	if slug == "" {
		return fallbackSlug
	}

	return slug
}

func collapseHyphens(raw string) string {
	var (
		builder  strings.Builder
		lastDash = true // baştaki tireler de düşsün
	)

	for i := range len(raw) {
		if raw[i] == '-' {
			if lastDash {
				continue
			}
			lastDash = true
			builder.WriteByte('-')

			continue
		}

		lastDash = false
		builder.WriteByte(raw[i])
	}

	return strings.TrimSuffix(builder.String(), "-")
}

// candidateSlug, çakışma durumunda denenecek slug'ı üretir.
//
// İlk denemeler okunabilir olsun diye sayı takısı (acme, acme-2, acme-3);
// birkaç denemeden sonra rastgele takıya geçiliyor. Gerekçe: popüler bir
// ad için sırayla artan sayı denemek, her yeni kayıtta bir öncekilerin
// hepsine çarpan bir döngü demek — rastgele takı ilk denemede bitiriyor.
func candidateSlug(base string, attempt int) (string, error) {
	switch {
	case attempt == 0:
		return base, nil

	case attempt < numberedSlugAttempts:
		return withSuffix(base, strconv.Itoa(attempt+1)), nil

	default:
		suffix, err := randomSuffix()
		if err != nil {
			return "", err
		}

		return withSuffix(base, suffix), nil
	}
}

const (
	// numberedSlugAttempts, sayı takılı denemelerin sayısı (acme-2 … acme-4).
	numberedSlugAttempts = 4
	// randomSuffixBytes, rastgele takının entropisi.
	randomSuffixBytes = 4
)

// withSuffix, slug'a "-<takı>" ekler; uzunluk sınırını aşıyorsa tabanı
// kısaltır (takı her zaman korunur, yoksa benzersizleştirme çalışmaz).
func withSuffix(base, suffix string) string {
	full := "-" + suffix

	if len(base)+len(full) > maxSlugLength {
		base = strings.Trim(base[:maxSlugLength-len(full)], "-")
	}

	return base + full
}

func randomSuffix() (string, error) {
	buf := make([]byte, randomSuffixBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	// base32 (küçük harfe çevrilmiş) yalnızca harf/rakam üretiyor — slug
	// kısıtıyla uyumlu. Doldurma karakteri (=) kapalı.
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}
