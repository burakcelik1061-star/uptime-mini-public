package monitors

import (
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Durum makinesinin eşik sınırları (şemadaki CHECK kısıtlarıyla aynı).
const (
	MinThreshold = 1
	MaxThreshold = 10

	// Varsayılanlar ASİMETRİK ve bilinçli: kesinti İLAN ETMEK pahalı (yanlış
	// alarm, gereksiz incident, gece uyanan biri), kesintinin BİTTİĞİNİ
	// söylemek ucuz.
	DefaultFailureThreshold  = 3
	DefaultRecoveryThreshold = 2
)

// State, durum makinesinin girdisi ve çıktısı: monitor'ün durumu + kararın
// hafızası olan ardışık sayaçlar.
type State struct {
	Status               store.MonitorStatus
	ConsecutiveFailures  int32
	ConsecutiveSuccesses int32
	FailureThreshold     int32
	RecoveryThreshold    int32
}

// StateOf, monitor kaydından durum makinesinin girdisini çıkarır.
func StateOf(m store.Monitor) State {
	return State{
		Status:               m.CurrentStatus,
		ConsecutiveFailures:  m.ConsecutiveFailures,
		ConsecutiveSuccesses: m.ConsecutiveSuccesses,
		FailureThreshold:     m.FailureThreshold,
		RecoveryThreshold:    m.RecoveryThreshold,
	}
}

// Transition, bir kontrol sonucunun durum üzerindeki etkisi.
type Transition struct {
	// From, sonuç uygulanmadan önceki durum.
	From store.MonitorStatus
	// Next, sonuç uygulandıktan sonraki DURUM + sayaçlar.
	Next State
	// Changed, durumun (sayaçların değil) değişip değişmediği. 4.1'in
	// incident açma/kapama kararı buna bakacak.
	Changed bool
}

// NextState, tek bir kontrol sonucunu uygular — SAF FONKSİYON.
//
// Saf olması bu adımın şartı: karar ne veritabanı ne saat ne de ağ görüyor,
// yani "5 ardışık hata + 1 başarı" gibi senaryolar gerçek zamanı beklemeden,
// tek satır testle ölçülebiliyor. Yan etkiler (yazma, loglama) çağıranda.
//
// KURALLAR
//
//	failure           → ardışık hata sayacı artar, başarı sayacı sıfırlanır.
//	                    Sayaç EŞİĞE ULAŞTIĞINDA durum `down` olur; öncesinde
//	                    durum DEĞİŞMEZ (flapping önleme: tek bir kayıp paket
//	                    kesinti ilan etmemeli).
//	success/degraded  → başarı sayacı artar, hata sayacı sıfırlanır.
//	                    Monitor `down` ise durum ancak KURTARMA EŞİĞİ kadar
//	                    ardışık yanıttan sonra değişir; değilse yanıtın türü
//	                    (yavaş mı değil mi) doğrudan yansır.
//
// DEGRADED NEDEN EŞİĞE TABİ DEĞİL (asimetri bilinçli): "yavaş" bir kesinti
// iddiası değil, incident açmıyor (4.1 `down`a bakıyor) ve zaten kontrol
// anında karara bağlanmış bir ölçüm (0.1 §5). Eşiğe bağlamak, üçüncü bir
// eşik ve "yavaşlığın ne zaman bittiği" için dördüncü bir kural demekti.
// Kesinti kararı ise pahalı olduğu için eşikli.
//
// `pending` (hiç kontrol edilmemiş) ilk YANITTA doğrudan `up`/`degraded`
// oluyor: kurtarma eşiği "kesintiden çıkış" kuralı, "ilk kez ölçüldü" değil.
// Yeni eklenen bir monitor'ün iki tur boyunca `pending` görünmesi, kullanıcının
// gözünde ürünün çalışmaması demekti.
//
// `paused` durumundaki monitor'e DOKUNULMUYOR: duraklatılmış monitor
// zamanlanmıyor (3.2), buraya ancak kullanıcı kontrol sürerken duraklattıysa
// gelinir — o kararı ezmek yanlış olurdu (yazma sorgusu ayrıca `is_active`
// şart koşuyor).
func NextState(current State, outcome store.CheckOutcome) Transition {
	next := current
	next.FailureThreshold = clampThreshold(current.FailureThreshold, DefaultFailureThreshold)
	next.RecoveryThreshold = clampThreshold(current.RecoveryThreshold, DefaultRecoveryThreshold)

	if current.Status == store.MonitorStatusPaused {
		return Transition{From: current.Status, Next: current}
	}

	switch outcome {
	case store.CheckOutcomeFailure:
		next.ConsecutiveFailures = current.ConsecutiveFailures + 1
		next.ConsecutiveSuccesses = 0

		if next.ConsecutiveFailures >= next.FailureThreshold {
			next.Status = store.MonitorStatusDown
		}

	case store.CheckOutcomeSuccess, store.CheckOutcomeDegraded:
		next.ConsecutiveSuccesses = current.ConsecutiveSuccesses + 1
		next.ConsecutiveFailures = 0

		responding := store.MonitorStatusUp
		if outcome == store.CheckOutcomeDegraded {
			responding = store.MonitorStatusDegraded
		}

		if current.Status == store.MonitorStatusDown {
			// Kesintiden çıkış EŞİĞE tabi: tek bir başarılı cevap, iki dakikada
			// bir düşüp kalkan bir servisi "çalışıyor" göstermemeli.
			if next.ConsecutiveSuccesses >= next.RecoveryThreshold {
				next.Status = responding
			}
		} else {
			next.Status = responding
		}
	}

	return Transition{
		From:    current.Status,
		Next:    next,
		Changed: next.Status != current.Status,
	}
}

// clampThreshold, eşik alanı hiç doldurulmamışsa (0) varsayılana düşer.
//
// Savunma amaçlı: şema 1-10 arasını zorluyor, yani üretimde 0 gelmemeli. Ama
// sıfır bir eşik "ilk kontrolde down" (veya sonsuz döngü) gibi sessiz ve
// tuhaf davranışlar üretirdi; saf fonksiyonun bozuk girdide de anlamlı
// davranması, testlerinin de güvenilir olması demek.
func clampThreshold(value, fallback int32) int32 {
	if value < MinThreshold {
		return fallback
	}

	if value > MaxThreshold {
		return MaxThreshold
	}

	return value
}
