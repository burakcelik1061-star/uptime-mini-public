package statuspage

// Sayfanın en üstündeki TEK SATIRLIK genel durum (5.1'in açık talebi).

import "github.com/burakcelik1061-star/uptime-mini/backend/internal/store"

// Overall, status page'in tek satırlık özeti.
//
// Sunucuda üretiliyor, istemcide değil: aynı hesabın ikinci bir kopyası
// (6.2'nin canlı güncellemesi, ileride bir mobil istemci) sessizce ayrışırsa
// aynı organizasyon iki farklı yerde "tüm sistemler çalışıyor" ve "kısmi
// kesinti" derdi. İstemci yalnızca metne çeviriyor.
type Overall string

const (
	// OverallOperational — yayımlanan her servis yanıt veriyor.
	OverallOperational Overall = "operational"
	// OverallDegraded — kesinti yok ama en az bir servis yavaş (3.3'ün
	// `degraded`ı: servis yanıt veriyor, bu yüzden kesinti sayılmıyor).
	OverallDegraded Overall = "degraded"
	// OverallPartialOutage — bazı servisler down.
	OverallPartialOutage Overall = "partial_outage"
	// OverallMajorOutage — durumu bilinen TÜM servisler down.
	OverallMajorOutage Overall = "major_outage"
	// OverallMaintenance — kesinti/yavaşlık yok ama en az bir servis planlı
	// bakımda.
	OverallMaintenance Overall = "maintenance"
	// OverallUnknown — durumu bilinen hiçbir servis yok (henüz kontrol
	// edilmemiş ya da duraklatılmış servisler, veya sayfa boş).
	OverallUnknown Overall = "unknown"
)

// Summarize, yayımlanan servislerin durumundan genel özeti üretir.
//
// SAF FONKSİYON (3.3'ün `NextState` disiplini): veritabanı, saat ve ağ
// görmüyor — "yedi servisin ikisi bakımda, biri down" gibi senaryolar gerçek
// veri kurmadan tek satırda test edilebiliyor.
//
// Üç kural, üçü de bilinçli:
//
//  1. BAKIMDAKİ SERVİS SAYIMA GİRMİYOR. 4.3'ten beri bakım penceresi "alarm
//     üretmeyi" durduruyor ama ölçmeyi durdurmuyor: bakımdaki bir monitor
//     gerçekten `down` görünür. Onu kesinti sayanan bir özet, planlı bakımı
//     ziyaretçiye arıza diye ilan ederdi.
//
//  2. `pending` VE `paused` SAYIMA GİRMİYOR. İkisi de "bilmiyoruz" demek;
//     "çalışıyor" saymak 0.1'in "veri yok, %100 değil" kuralının aynı
//     ihlali olurdu, "down" saymak ise olmayan bir kesinti ilan etmek.
//
//  3. BAYAT SERVİS DE SAYIMA GİRMİYOR (8.6, kullanıcı kararı). Ölçümü durmuş
//     bir servisin `current_status`u SON BİLİNEN durumu taşıyor; onu
//     "çalışıyor" saymak, worker öldüğünde sayfanın "tüm sistemler çalışıyor"
//     demeye devam etmesi demekti — bu ürünün en pahalı arıza modu. "down"
//     saymak da yanlış olurdu: olmayan bir kesinti ilan etmek. Yani `pending`
//     ve `paused` ile AYNI sınıf: bilmiyoruz.
//
// OVERALL ENUM'UNA YENİ DEĞER EKLENMEDİ (kullanıcı kararı): 5.1'in sözleşmesi
// olduğu gibi duruyor ve TÜM servisler bayatsa sonuç zaten `unknown` çıkıyor.
// Karışık durumda (üç sağlam + bir bayat) özet `operational` kalıyor — satır
// dürüst ama BAŞLIK değil; o boşluğu `StaleServices` sayacı ve sayfa üstündeki
// not kapatıyor (bkz. PublicPage.StaleServices).
func Summarize(services []PublicService) Overall {
	var (
		known       int
		down        int
		degraded    int
		maintaining int
	)

	for _, service := range services {
		if service.InMaintenance {
			maintaining++
			continue
		}

		if service.Stale {
			// Bilmiyoruz: hiçbir sayaca girmiyor (pending/paused gibi).
			continue
		}

		switch service.Status {
		case store.MonitorStatusDown:
			known++
			down++
		case store.MonitorStatusDegraded:
			known++
			degraded++
		case store.MonitorStatusUp:
			known++
		case store.MonitorStatusPending, store.MonitorStatusPaused:
			// Bilinmiyor: hiçbir sayaca girmiyor.
		}
	}

	switch {
	case down > 0 && down == known:
		return OverallMajorOutage
	case down > 0:
		return OverallPartialOutage
	case degraded > 0:
		return OverallDegraded
	case maintaining > 0:
		return OverallMaintenance
	case known > 0:
		return OverallOperational
	default:
		return OverallUnknown
	}
}
