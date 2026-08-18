// Package plans, planların ne vaat ettiğini tanımlar (Faz 8.5).
//
// AYRI BİR PAKET çünkü iki farklı katman soruyor: `internal/monitors` kotayı
// UYGULUYOR, `internal/httpapi` kullanıcıya GÖSTERİYOR. Limitleri
// monitors'ün içine gömmek, plan ekranının bir iş mantığı paketinden değer
// okuması demekti; orgs'a koymak ise monitors → orgs bağımlılığı eklerdi.
//
// # Sayılar bir ÜRÜN kararı (kullanıcı kararı) ve gerekçesi şu
//
//  1. ASIL EKSEN ARALIK, SAYI DEĞİL. Maliyeti FREKANS yaratıyor: 30 saniyelik
//     bir monitor, 300 saniyeliğin ON KATI kontrol üretiyor — worker
//     eşzamanlılığı (3.2), `check_results` satır sayısı (2.3), 3.4'ün rollup
//     işi ve partition büyümesi hep frekansla ölçekleniyor, monitor sayısıyla
//     değil. Gerçekten kaynak tüketen şeyi kapıya koymak, keyfi bir sayı
//     seçmekten savunulabilir.
//
//  2. REFERANS ÜRÜNLE AYNI ŞEKİL. Projenin referans ürünü UptimeRobot:
//     ücretsiz plan cömert sayı + 5 dakikalık aralık, ücretli plan dakika
//     altı. 300 → 30 saniye ON KAT çözünürlük farkı, yani kozmetik değil
//     gerçek bir ürün farkı. 60 → 30 olsaydı yalnızca 2× olurdu ve 30 saniye
//     zaten şemanın alt sınırı (PRO daha hızlı gidemiyor), yani o eksende
//     oynanabilecek tek yer FREE tarafı.
//
//  3. SAYI SINIRI DA VAR AMA İKİNCİL (5/50): kötüye kullanımı sınırlıyor,
//     planı satan şey aralık.
package plans

import (
	"fmt"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Limits, bir planın sınırları.
type Limits struct {
	// MaxMonitors, organizasyonun açabileceği en fazla monitor sayısı.
	MaxMonitors int
	// MinCheckIntervalSeconds, kontrol aralığının alt sınırı.
	//
	// Şemanın 30 saniyelik mutlak sınırının (migration 00005) ÜSTÜNE biniyor:
	// plan bu değerin altına inemiyor, şema da 30'un altına hiç izin vermiyor.
	MinCheckIntervalSeconds int32
}

// limits, plan → sınır eşlemesi.
//
// TANINMAYAN PLAN YOK: `For` bilinmeyen bir değer için FREE'nin sınırlarını
// döndürüyor, yani "bilmiyorum" hâli en DAR tarafa düşüyor. 1.4'ün rol
// sıralamasındaki kuralın aynısı — belirsizlikte cömert davranmak, enum'a
// eklenip buraya yazılmayan bir planın sınırsız olması demekti.
var limits = map[store.OrgPlan]Limits{
	store.OrgPlanFree: {
		MaxMonitors:             5,
		MinCheckIntervalSeconds: 300,
	},
	store.OrgPlanPro: {
		MaxMonitors:             50,
		MinCheckIntervalSeconds: 30,
	},
}

// For, planın sınırlarını döner.
func For(plan store.OrgPlan) Limits {
	if l, ok := limits[plan]; ok {
		return l
	}

	return limits[store.OrgPlanFree]
}

// Label, planın kullanıcıya gösterilen adı.
func Label(plan store.OrgPlan) string {
	switch plan {
	case store.OrgPlanPro:
		return "Pro"
	case store.OrgPlanFree:
		return "Ücretsiz"
	default:
		return string(plan)
	}
}

// Parse, istemciden gelen plan metnini enum'a çevirir.
//
// Tanınmayan değer HATA, sessizce bir varsayılana düşmüyor (1.4'ün
// "tanımıyorum ile izin ver aynı şey değil" kuralı).
func Parse(raw string) (store.OrgPlan, error) {
	switch store.OrgPlan(raw) {
	case store.OrgPlanFree:
		return store.OrgPlanFree, nil
	case store.OrgPlanPro:
		return store.OrgPlanPro, nil
	default:
		return "", fmt.Errorf("tanınmayan plan: %q", raw)
	}
}

// All, planların gösterim sırası (plan ekranı için).
func All() []store.OrgPlan {
	return []store.OrgPlan{store.OrgPlanFree, store.OrgPlanPro}
}
