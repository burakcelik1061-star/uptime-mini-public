package monitors

import (
	"testing"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Durum makinesi SAF bir fonksiyon olduğu için testleri de saf: veritabanı,
// saat ve ağ yok. "5 ardışık hata + 1 başarı" gibi senaryolar gerçek zamanı
// beklemeden ölçülüyor — bu adımın şartı buydu.

func state(status store.MonitorStatus, failures, successes int32) State {
	return State{
		Status:               status,
		ConsecutiveFailures:  failures,
		ConsecutiveSuccesses: successes,
		FailureThreshold:     DefaultFailureThreshold,
		RecoveryThreshold:    DefaultRecoveryThreshold,
	}
}

// apply, sonuç dizisini sırayla uygular (senaryo testleri için).
func apply(start State, outcomes ...store.CheckOutcome) State {
	current := start
	for _, outcome := range outcomes {
		current = NextState(current, outcome).Next
	}

	return current
}

const (
	ok       = store.CheckOutcomeSuccess
	slow     = store.CheckOutcomeDegraded
	fail     = store.CheckOutcomeFailure
	statusUp = store.MonitorStatusUp
)

// FLAPPING ÖNLEMENİN ÖZÜ: tek bir başarısız kontrol kesinti İLAN ETMİYOR.
func TestSingleFailureDoesNotGoDown(t *testing.T) {
	transition := NextState(state(statusUp, 0, 5), fail)

	if transition.Next.Status != statusUp {
		t.Errorf("durum = %q, beklenen up (eşik aşılmadı)", transition.Next.Status)
	}
	if transition.Changed {
		t.Error("durum değişmedi ama Changed=true")
	}
	if transition.Next.ConsecutiveFailures != 1 {
		t.Errorf("ardışık hata = %d, beklenen 1", transition.Next.ConsecutiveFailures)
	}
	if transition.Next.ConsecutiveSuccesses != 0 {
		t.Errorf("başarı sayacı sıfırlanmadı: %d", transition.Next.ConsecutiveSuccesses)
	}
}

// EŞİĞE ULAŞILDIĞINDA (öncesinde değil) DOWN.
func TestGoesDownExactlyAtThreshold(t *testing.T) {
	current := state(statusUp, 0, 3)

	for i := 1; i <= DefaultFailureThreshold; i++ {
		transition := NextState(current, fail)
		current = transition.Next

		wantDown := i >= DefaultFailureThreshold
		isDown := current.Status == store.MonitorStatusDown

		if isDown != wantDown {
			t.Fatalf("%d. hatada durum = %q (down beklentisi: %v)", i, current.Status, wantDown)
		}

		if transition.Changed != (i == DefaultFailureThreshold) {
			t.Errorf("%d. hatada Changed = %v", i, transition.Changed)
		}
	}
}

// Araya giren TEK bir başarı sayacı sıfırlıyor: "2 hata + 1 başarı + 2 hata"
// kesinti değil. Sayaç sıfırlanmasaydı gürültülü bir servis günün sonunda
// mutlaka DOWN olurdu.
func TestSuccessResetsFailureStreak(t *testing.T) {
	current := apply(state(statusUp, 0, 1), fail, fail, ok, fail, fail)

	if current.Status != statusUp {
		t.Errorf("durum = %q, beklenen up", current.Status)
	}
	if current.ConsecutiveFailures != 2 {
		t.Errorf("ardışık hata = %d, beklenen 2", current.ConsecutiveFailures)
	}
}

// KESİNTİDEN ÇIKIŞ EŞİĞE TABİ: tek bir başarılı cevap, iki dakikada bir düşüp
// kalkan bir servisi "çalışıyor" göstermemeli.
func TestRecoveryNeedsThreshold(t *testing.T) {
	down := apply(state(statusUp, 0, 0), fail, fail, fail)
	if down.Status != store.MonitorStatusDown {
		t.Fatalf("hazırlık: durum = %q, beklenen down", down.Status)
	}

	first := NextState(down, ok)
	if first.Next.Status != store.MonitorStatusDown {
		t.Errorf("tek başarıda durum = %q, hâlâ down olmalıydı", first.Next.Status)
	}
	if first.Changed {
		t.Error("durum değişmedi ama Changed=true")
	}

	second := NextState(first.Next, ok)
	if second.Next.Status != statusUp {
		t.Errorf("kurtarma eşiğinde durum = %q, beklenen up", second.Next.Status)
	}
	if !second.Changed {
		t.Error("durum değişti ama Changed=false")
	}
}

// Kurtarma sırasında gelen bir hata sayacı sıfırlıyor: yarım kurtarma
// "kurtuldu" sayılmamalı.
func TestFailureDuringRecoveryResetsSuccessStreak(t *testing.T) {
	down := apply(state(statusUp, 0, 0), fail, fail, fail)
	current := apply(down, ok, fail)

	if current.Status != store.MonitorStatusDown {
		t.Errorf("durum = %q, beklenen down", current.Status)
	}
	if current.ConsecutiveSuccesses != 0 {
		t.Errorf("başarı sayacı = %d, sıfırlanmalıydı", current.ConsecutiveSuccesses)
	}
	// Hata sayacı kaldığı yerden değil, kesintiden bu yana sayılıyor.
	if current.ConsecutiveFailures != 1 {
		t.Errorf("ardışık hata = %d, beklenen 1", current.ConsecutiveFailures)
	}
}

// DEGRADED EŞİĞE TABİ DEĞİL (bilinçli asimetri): "yavaş" bir kesinti iddiası
// değil, incident açmıyor ve zaten kontrol anında karara bağlanmış bir ölçüm.
func TestDegradedAppliesImmediately(t *testing.T) {
	transition := NextState(state(statusUp, 0, 10), slow)

	if transition.Next.Status != store.MonitorStatusDegraded {
		t.Errorf("durum = %q, beklenen degraded", transition.Next.Status)
	}
	if !transition.Changed {
		t.Error("durum değişti ama Changed=false")
	}
	// Yanıt veren bir servis: hata sayacı sıfırlanıyor, başarı sayacı artıyor
	// (0.1'in uptime tanımında DEGRADED "up" sayılıyor).
	if transition.Next.ConsecutiveFailures != 0 || transition.Next.ConsecutiveSuccesses != 11 {
		t.Errorf("sayaçlar = %d hata / %d başarı", transition.Next.ConsecutiveFailures,
			transition.Next.ConsecutiveSuccesses)
	}
}

// Yavaşlık geçtiğinde durum kendiliğinden up'a dönüyor.
func TestDegradedRecoversToUp(t *testing.T) {
	current := apply(state(statusUp, 0, 1), slow, ok)

	if current.Status != statusUp {
		t.Errorf("durum = %q, beklenen up", current.Status)
	}
}

// DOWN'dan çıkarken son kontrol YAVAŞSA durum `degraded` oluyor: "çalışıyor"
// demek yanlış olurdu, ölçüm yavaş olduğunu söylüyor.
func TestRecoveryFromDownWithDegradedResult(t *testing.T) {
	down := apply(state(statusUp, 0, 0), fail, fail, fail)
	current := apply(down, slow, slow)

	if current.Status != store.MonitorStatusDegraded {
		t.Errorf("durum = %q, beklenen degraded", current.Status)
	}
}

// YENİ MONITOR (pending) İLK YANITTA up: kurtarma eşiği "kesintiden çıkış"
// kuralı, "ilk kez ölçüldü" değil. Aksi hâlde yeni eklenen monitor iki tur
// boyunca 'pending' görünürdü — kullanıcının gözünde ürün çalışmıyor demek.
func TestPendingBecomesUpOnFirstSuccess(t *testing.T) {
	transition := NextState(state(store.MonitorStatusPending, 0, 0), ok)

	if transition.Next.Status != statusUp {
		t.Errorf("durum = %q, beklenen up", transition.Next.Status)
	}
	if !transition.Changed {
		t.Error("pending → up değişimi bildirilmedi")
	}
}

// Yeni monitor'ün İLK kontrolü başarısızsa da hemen DOWN olmuyor: eşik burada
// da geçerli (aksi hâlde yanlış yazılmış tek bir URL anında kesinti üretirdi).
func TestPendingStaysPendingUntilFailureThreshold(t *testing.T) {
	current := NextState(state(store.MonitorStatusPending, 0, 0), fail).Next
	if current.Status != store.MonitorStatusPending {
		t.Errorf("durum = %q, beklenen pending", current.Status)
	}

	current = apply(current, fail, fail)
	if current.Status != store.MonitorStatusDown {
		t.Errorf("durum = %q, beklenen down", current.Status)
	}
}

// DURAKLATILMIŞ monitor'e dokunulmuyor: duraklatılmış monitor zaten
// zamanlanmıyor (3.2); buraya ancak kullanıcı kontrol sürerken duraklattıysa
// gelinir ve o kararı ezmek yanlış olurdu.
func TestPausedIsNeverChanged(t *testing.T) {
	paused := state(store.MonitorStatusPaused, 2, 0)

	for _, outcome := range []store.CheckOutcome{ok, slow, fail} {
		transition := NextState(paused, outcome)

		if transition.Next.Status != store.MonitorStatusPaused {
			t.Errorf("%q sonucunda durum = %q", outcome, transition.Next.Status)
		}
		if transition.Changed {
			t.Errorf("%q sonucunda Changed=true", outcome)
		}
		if transition.Next != paused {
			t.Errorf("%q sonucunda sayaçlar değişti: %+v", outcome, transition.Next)
		}
	}
}

// EŞİKLER MONITOR BAZINDA: kritik bir uçta 1 hata yeterken, gürültülü bir
// üçüncü parti serviste 5 ardışık hata istemek meşru.
func TestPerMonitorThresholds(t *testing.T) {
	strict := State{
		Status: statusUp, FailureThreshold: 1, RecoveryThreshold: 1,
	}

	if got := NextState(strict, fail).Next.Status; got != store.MonitorStatusDown {
		t.Errorf("eşik 1'de tek hatada durum = %q, beklenen down", got)
	}

	tolerant := State{
		Status: statusUp, FailureThreshold: 5, RecoveryThreshold: 3,
	}

	current := apply(tolerant, fail, fail, fail, fail)
	if current.Status != statusUp {
		t.Errorf("eşik 5'te 4 hatadan sonra durum = %q, beklenen up", current.Status)
	}

	current = apply(current, fail)
	if current.Status != store.MonitorStatusDown {
		t.Errorf("eşik 5'te 5. hatada durum = %q, beklenen down", current.Status)
	}

	// Kurtarma eşiği 3: iki başarı yetmiyor.
	current = apply(current, ok, ok)
	if current.Status != store.MonitorStatusDown {
		t.Errorf("kurtarma eşiği 3 iken 2 başarıda durum = %q", current.Status)
	}

	current = apply(current, ok)
	if current.Status != statusUp {
		t.Errorf("kurtarma eşiği 3 iken 3 başarıda durum = %q", current.Status)
	}
}

// Bozuk/eksik eşikler (0 veya sınır dışı) makineyi tuhaf davranışlara
// sürüklememeli: şema zaten 1-10 zorluyor, ama saf fonksiyon kendi başına da
// anlamlı davranıyor.
func TestThresholdsAreClamped(t *testing.T) {
	zero := State{Status: statusUp} // eşikler 0

	transition := NextState(zero, fail)
	if transition.Next.FailureThreshold != DefaultFailureThreshold {
		t.Errorf("hata eşiği = %d, varsayılana düşmeliydi", transition.Next.FailureThreshold)
	}
	if transition.Next.Status != statusUp {
		t.Errorf("sıfır eşikle tek hatada durum = %q (ilk kontrolde down olmamalı)",
			transition.Next.Status)
	}

	huge := State{Status: statusUp, FailureThreshold: 999, RecoveryThreshold: 999}
	if got := NextState(huge, fail).Next.FailureThreshold; got != MaxThreshold {
		t.Errorf("hata eşiği = %d, üst sınıra çekilmeliydi (%d)", got, MaxThreshold)
	}
}

// Uzun bir senaryo: sağlıklı → kesinti → kurtarma → yavaşlama.
func TestFullLifecycle(t *testing.T) {
	steps := []struct {
		outcome store.CheckOutcome
		want    store.MonitorStatus
	}{
		{ok, statusUp},
		{ok, statusUp},
		{fail, statusUp},                    // 1. hata: henüz değil
		{fail, statusUp},                    // 2. hata: henüz değil
		{fail, store.MonitorStatusDown},     // 3. hata: kesinti
		{fail, store.MonitorStatusDown},     // sürüyor
		{ok, store.MonitorStatusDown},       // 1. kurtarma: henüz değil
		{ok, statusUp},                      // 2. kurtarma: geri geldi
		{slow, store.MonitorStatusDegraded}, // yavaşladı
		{slow, store.MonitorStatusDegraded}, // hâlâ yavaş
		{ok, statusUp},                      // toparladı
		{fail, statusUp},                    //nolint:godot // eşik yeniden 3'ten başlıyor
	}

	current := state(store.MonitorStatusPending, 0, 0)
	for i, step := range steps {
		current = NextState(current, step.outcome).Next

		if current.Status != step.want {
			t.Fatalf("%d. adım (%s): durum = %q, beklenen %q",
				i+1, step.outcome, current.Status, step.want)
		}
	}
}
