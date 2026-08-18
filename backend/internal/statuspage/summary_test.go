package statuspage

import (
	"testing"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Summarize SAF bir fonksiyon (3.3'ün NextState'i gibi): veritabanı, saat ve ağ
// görmüyor. Bu yüzden "yedi servisin ikisi bakımda, biri down" gibi senaryolar
// gerçek veri kurmadan tek satırda ölçülebiliyor.

func service(status store.MonitorStatus, inMaintenance bool) PublicService {
	return PublicService{Status: status, InMaintenance: inMaintenance}
}

// staleService, ölçümü DURMUŞ bir servis (8.6).
func staleService(status store.MonitorStatus) PublicService {
	return PublicService{Status: status, Stale: true}
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name     string
		services []PublicService
		want     Overall
	}{
		{
			name: "hepsi çalışıyorsa operational",
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				service(store.MonitorStatusUp, false),
			},
			want: OverallOperational,
		},
		{
			// DEGRADED "up" sayılıyor (0.1'in bağlayıcı kararı) ama genel özet
			// yine de bunu söylüyor: servis yanıt veriyor, sadece yavaş.
			name: "tek yavaş servis degraded",
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				service(store.MonitorStatusDegraded, false),
			},
			want: OverallDegraded,
		},
		{
			name: "bir servis down ise kısmi kesinti",
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				service(store.MonitorStatusDown, false),
			},
			want: OverallPartialOutage,
		},
		{
			name: "bilinen tüm servisler down ise büyük kesinti",
			services: []PublicService{
				service(store.MonitorStatusDown, false),
				service(store.MonitorStatusDown, false),
			},
			want: OverallMajorOutage,
		},
		{
			// Kesinti yavaşlığı bastırıyor: aynı anda hem "yavaş" hem "kesinti"
			// demek okuyucuya tek satırda anlatılamaz, ciddi olan kazanır.
			name: "down ve degraded birlikteyse kesinti kazanır",
			services: []PublicService{
				service(store.MonitorStatusDown, false),
				service(store.MonitorStatusDegraded, false),
			},
			want: OverallPartialOutage,
		},
		{
			// 4.3'ün kuralının public karşılığı: bakım penceresi "ölçmeyi"
			// durdurmuyor, yani bakımdaki monitor gerçekten `down` görünür.
			// Onu kesinti saymak planlı bakımı arıza gibi ilan etmek olurdu.
			name: "bakımdaki down servis kesinti sayılmıyor",
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				service(store.MonitorStatusDown, true),
			},
			want: OverallMaintenance,
		},
		{
			name: "bakım dışında gerçek kesinti varsa kesinti kazanır",
			services: []PublicService{
				service(store.MonitorStatusDown, false),
				service(store.MonitorStatusDown, true),
			},
			want: OverallMajorOutage,
		},
		{
			// "Bilmiyoruz" ile "çalışıyor" ayrı: 0.1'in "veri yok, %100 değil"
			// kuralının durum tarafındaki karşılığı.
			name: "yalnızca pending/paused servisler unknown",
			services: []PublicService{
				service(store.MonitorStatusPending, false),
				service(store.MonitorStatusPaused, false),
			},
			want: OverallUnknown,
		},
		{
			name: "duraklatılmış servis çalışan servisin özetini bozmuyor",
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				service(store.MonitorStatusPaused, false),
			},
			want: OverallOperational,
		},
		{
			// Tek bilinen servis down ise bu "büyük kesinti": yayımlanan her
			// şey kapalı.
			name: "tek servis down ise büyük kesinti",
			services: []PublicService{
				service(store.MonitorStatusDown, false),
				service(store.MonitorStatusPending, false),
			},
			want: OverallMajorOutage,
		},
		{
			name:     "hiç servis yoksa unknown",
			services: nil,
			want:     OverallUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Summarize(tt.services); got != tt.want {
				t.Errorf("Summarize() = %q, beklenen %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeSlug(t *testing.T) {
	tests := map[string]string{
		"acme":     "acme",
		"  acme  ": "acme",
		// 5.4'te alt alan adından gelecek; alan adları büyük/küçük harf duyarsız.
		"ACME":   "acme",
		"Acme-2": "acme-2",
		"   ":    "",
	}

	for raw, want := range tests {
		if got := NormalizeSlug(raw); got != want {
			t.Errorf("NormalizeSlug(%q) = %q, beklenen %q", raw, got, want)
		}
	}
}

func TestValidateServices(t *testing.T) {
	// Boş liste GEÇERLİ: "hiçbir servisi yayımlamıyorum" meşru bir durum.
	if err := ValidateServices(ServicesInput{}); err != nil {
		t.Errorf("boş liste reddedildi: %v", err)
	}

	id := uuid.New()

	tests := map[string]ServicesInput{
		"yinelenen servis": {Items: []ItemInput{{MonitorID: id}, {MonitorID: id}}},
		"yinelenen grup":   {Groups: []string{"API", "API"}},
		"boş grup adı":     {Groups: []string{" "}},
		// Tanınmayan grup sessizce "gruplanmamış"a düşmemeli.
		"tanınmayan grup": {
			Groups: []string{"API"},
			Items:  []ItemInput{{MonitorID: id, Group: "Web"}},
		},
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateServices(NormalizeServices(in)); err == nil {
				t.Error("geçersiz girdi kabul edildi")
			}
		})
	}

	// Gruplu ve gruplanmamış servisler bir arada geçerli.
	valid := ServicesInput{
		Groups: []string{"API", "Web"},
		Items: []ItemInput{
			{MonitorID: uuid.New(), Group: "API"},
			{MonitorID: uuid.New()},
		},
	}
	if err := ValidateServices(NormalizeServices(valid)); err != nil {
		t.Errorf("geçerli girdi reddedildi: %v", err)
	}
}

// BAYAT SERVİS ÖZETE GİRMİYOR (8.6) — bu fazın en önemli davranışı.
//
// worker öldüğünde `current_status` son bilinen durumu taşımaya devam ediyor.
// Bayat bir `up` servisi "çalışıyor" saymak, sayfanın hiçbir şey ölçülmüyorken
// "tüm sistemler çalışıyor" demesi demekti — bu ürünün en pahalı arıza modu.
func TestSummarizeExcludesStaleServices(t *testing.T) {
	tests := map[string]struct {
		services []PublicService
		want     Overall
	}{
		"tek servis ve bayat → bilinmiyor": {
			// SON BİLİNEN DURUM `up` ama ölçüm durmuş: "çalışıyor" demek yanlış.
			services: []PublicService{staleService(store.MonitorStatusUp)},
			want:     OverallUnknown,
		},
		"hepsi bayat → bilinmiyor": {
			services: []PublicService{
				staleService(store.MonitorStatusUp),
				staleService(store.MonitorStatusUp),
			},
			want: OverallUnknown,
		},
		"bayat servis sağlamları GÖLGELEMİYOR": {
			// Karışık durumda özet `operational` kalıyor ve bu bilinçli:
			// `Overall` sözleşmesine (5.1) yeni bir değer eklenmedi. Başlığın
			// eksik kalan tarafını sayfa üstündeki not kapatıyor
			// (PublicPage.StaleServices).
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				staleService(store.MonitorStatusUp),
			},
			want: OverallOperational,
		},
		"bayat servis KESİNTİ de ilan etmiyor": {
			// "Bilmiyoruz" ile "kesinti var" ayrı şeyler: bayat bir `down`
			// servisi kesinti saymak, olmayan bir kesinti ilan etmek olurdu.
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				staleService(store.MonitorStatusDown),
			},
			want: OverallOperational,
		},
		"bayat OLMAYAN down servis kesinti": {
			services: []PublicService{
				service(store.MonitorStatusUp, false),
				service(store.MonitorStatusDown, false),
			},
			want: OverallPartialOutage,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Summarize(tc.services); got != tc.want {
				t.Errorf("Summarize = %q, beklenen %q", got, tc.want)
			}
		})
	}
}

// countStale, sayfa üstündeki notu besleyen sayaç.
func TestCountStale(t *testing.T) {
	services := []PublicService{
		service(store.MonitorStatusUp, false),
		staleService(store.MonitorStatusUp),
		staleService(store.MonitorStatusDown),
	}

	if got := countStale(services); got != 2 {
		t.Errorf("countStale = %d, beklenen 2", got)
	}
}
