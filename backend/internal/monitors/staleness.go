package monitors

// BAYATLIK (Faz 8.6) — bu fazın en önemli maddesi.
//
// # Kapatılan körlük
//
// worker ölürse kontroller durur ama `monitors.current_status` SON BİLİNEN
// durumu taşımaya devam eder (0.1 §5'in denormalize sütunu). Sonuç, bir
// monitoring ürününün EN PAHALI arıza modu: pano ve public status sayfası
// yeşil göstermeye devam eder, kimse hiçbir şey ölçmüyorken. Bu arızanın
// hiçbir dış sağlayıcıyla ilgisi yok — tamamen bizim tasarımımızın sonucu.
//
// # Yeni sütun EKLENMEDİ
//
// "Son kontrolden bu yana geçen süre" 3.2'den beri şemada duran
// `monitors.last_checked_at`ten TÜRETİLEBİLİYOR. Bir `is_stale` sütunu
// eklemek, doğruluğu bir job'ın tazeliğine bağlı İKİNCİ bir denormalize alan
// demekti — üstelik o job'ın kendisi worker'da, yani tam da ölçmeyi bıraktığı
// varsayılan süreçte.
//
// # Eşik UYDURULMADI
//
// Çarpan 3 ve kaynağı projenin kendi içinde: 3.1'in kalp atışı healthcheck'i
// worker'ı `periyot × 3` ile ölü sayıyor ve gerekçesi orada yazılı — "tek
// gecikmiş yazma flap üretmesin, donma hızla görünsün". Aynı soru, aynı
// cevap: bir monitor kendi `check_interval_seconds` değerinin üç katı
// boyunca ölçülmediyse, bu artık gecikme değil DURMUŞLUK.

import "time"

// StaleIntervalMultiplier, bir monitor'ün kaç kontrol aralığı boyunca
// ölçülmezse bayat sayılacağı.
//
// 3.1'in kalp atışı toleransıyla AYNI (`periyot × 3`) ve bilinçli olarak
// yapılandırılabilir DEĞİL: ortam değişkeni yapmak, "kaç turda alarm
// vereyim" sorusunu operatöre devretmek ve varsayılanı yanlış ayarlanmış bir
// kurulumda körlüğü geri getirmek olurdu.
const StaleIntervalMultiplier = 3

// StaleAfter, monitor'ün bayat sayılacağı süre.
func StaleAfter(checkIntervalSeconds int32) time.Duration {
	return time.Duration(checkIntervalSeconds) * time.Second * StaleIntervalMultiplier
}

// IsStale, monitor'ün ölçümünün DURMUŞ olup olmadığını söyler.
//
// SAF FONKSİYON (3.3'ün `NextState` disiplini): veritabanı, saat ve ağ
// görmüyor — "şimdi" ÇAĞIRANDAN geliyor. Sebep 8.6'ya özgü ve önemli:
// `last_checked_at` worker tarafından veritabanının `now()`u ile yazılıyor,
// yani onunla karşılaştırılacak "şimdi" de VERİTABANININ saati olmalı. api
// sürecinin `time.Now()`u ile karşılaştırmak, iki konteyner arasındaki saat
// kaymasını bayatlığa çevirirdi (3.2'nin "zamanın kaynağı veritabanı"
// kararının okuma tarafındaki karşılığı; 7.5 aynı hatayı public cevabın
// "şimdi"si için düzeltmişti).
//
// ÜÇ YANLIŞ ALARM KAPISI, üçü de burada:
//
//  1. DURAKLATILMIŞ monitor bayat DEĞİL. Kullanıcı ölçmeyi bilerek durdurdu;
//     durumu zaten `paused` ve 3.2'den beri sırası (`next_check_at`) NULL.
//     Bayat saymak, kullanıcının kendi kararını arıza diye ilan etmekti.
//
//  2. HİÇ KONTROL EDİLMEMİŞ monitor bayat DEĞİL (`lastCheckedAt == nil`).
//     "Hiç ölçülmedi" ile "ölçüm durdu" farklı şeyler: ilkinin durumu zaten
//     `pending` ve kullanıcı onu ekleyeli birkaç saniye olmuş olabilir.
//
//  3. Eşik monitor'ün KENDİ aralığından türüyor: 30 saniyede bir kontrol
//     edilen bir servis 90 saniyede bayat, günde bir kontrol edilen servis
//     üç günde. Tek bir sabit eşik, seyrek kontrol edilen her monitor'ü
//     sürekli bayat gösterirdi.
func IsStale(isActive bool, lastCheckedAt *time.Time, checkIntervalSeconds int32, now time.Time) bool {
	if !isActive || lastCheckedAt == nil {
		return false
	}

	return now.Sub(*lastCheckedAt) > StaleAfter(checkIntervalSeconds)
}
