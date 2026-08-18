package orgs

import (
	"regexp"
	"strings"
	"testing"
)

// slugPattern, migration'daki CHECK kısıtının aynısı. slugify'ın ürettiği
// her değer bu kalıba uymak zorunda; uymazsa hata veritabanından dönerdi.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestSlugify(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"basit":                   {"Acme Corp", "acme-corp"},
		"zaten slug":              {"acme-corp", "acme-corp"},
		"türkçe harfler":          {"Şirket Adı", "sirket-adi"},
		"türkçe büyük I":          {"IŞIK Teknoloji", "isik-teknoloji"},
		"aksanlı harfler":         {"Café Ñandú", "cafe-nandu"},
		"noktalama":               {"Acme, Inc. (2026)!", "acme-inc-2026"},
		"fazla boşluk":            {"   Acme    Corp   ", "acme-corp"},
		"art arda tire":           {"acme---corp", "acme-corp"},
		"baş/son tire":            {"--acme--", "acme"},
		"rakamla başlayan":        {"7/24 İzleme", "7-24-izleme"},
		"emoji":                   {"Acme 🚀 Corp", "acme-corp"},
		"tamamen çevrilemeyen":    {"日本語", fallbackSlug},
		"tamamen noktalama":       {"!!!", fallbackSlug},
		"boş":                     {"", fallbackSlug},
		"yalnızca boşluk":         {"   ", fallbackSlug},
		"alt çizgi ayraç sayılır": {"acme_corp", "acme-corp"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := slugify(tc.in)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, beklenen %q", tc.in, got, tc.want)
			}
			if !slugPattern.MatchString(got) {
				t.Errorf("slug veritabanı kısıtına uymuyor: %q", got)
			}
		})
	}
}

// Uzun ad kesiliyor ama sonuç yine geçerli bir slug olmalı (sonunda tire
// kalmamalı).
func TestSlugifyTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("uptime izleme servisi ", 10)

	got := slugify(long)
	if len(got) > maxSlugLength {
		t.Errorf("slug uzunluğu = %d, üst sınır %d (%q)", len(got), maxSlugLength, got)
	}
	if !slugPattern.MatchString(got) {
		t.Errorf("kesilen slug kısıta uymuyor: %q", got)
	}

	// Tam kesme noktasında tire olan bir ad da geçerli sonuç vermeli.
	atBoundary := strings.Repeat("ab-", 20)
	if got := slugify(atBoundary); !slugPattern.MatchString(got) {
		t.Errorf("kesme noktasında tire kalmış: %q", got)
	}
}

func TestCandidateSlugProducesDistinctValidSlugs(t *testing.T) {
	base := slugify("Acme Corp")

	seen := map[string]bool{}
	for attempt := range maxSlugCollisionAttempts {
		got, err := candidateSlug(base, attempt)
		if err != nil {
			t.Fatalf("deneme %d: beklenmeyen hata: %v", attempt, err)
		}
		if !slugPattern.MatchString(got) {
			t.Errorf("deneme %d: kısıta uymayan slug %q", attempt, got)
		}
		if len(got) > maxSlugLength {
			t.Errorf("deneme %d: slug çok uzun (%d): %q", attempt, len(got), got)
		}
		if seen[got] {
			t.Errorf("deneme %d: daha önce üretilmiş slug: %q", attempt, got)
		}
		seen[got] = true
	}

	// İlk denemeler okunabilir olsun.
	if first, _ := candidateSlug(base, 0); first != "acme-corp" {
		t.Errorf("ilk deneme = %q, beklenen acme-corp", first)
	}
	if second, _ := candidateSlug(base, 1); second != "acme-corp-2" {
		t.Errorf("ikinci deneme = %q, beklenen acme-corp-2", second)
	}
}

// Takı, uzunluk sınırına dayanmış bir tabanda da korunmalı: aksi hâlde
// benzersizleştirme çalışmaz.
func TestCandidateSlugKeepsSuffixOnLongBase(t *testing.T) {
	base := slugify(strings.Repeat("uzun-ad-", 20))

	for _, attempt := range []int{1, numberedSlugAttempts} {
		got, err := candidateSlug(base, attempt)
		if err != nil {
			t.Fatalf("beklenmeyen hata: %v", err)
		}
		if len(got) > maxSlugLength {
			t.Errorf("slug çok uzun (%d): %q", len(got), got)
		}
		if got == base {
			t.Errorf("deneme %d takı eklemedi: %q", attempt, got)
		}
		if !slugPattern.MatchString(got) {
			t.Errorf("kısıta uymayan slug: %q", got)
		}
	}
}
