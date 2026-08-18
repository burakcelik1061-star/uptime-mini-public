package seed

// FAZ 8'İN YÜZEYLERİ DEMO VERİDE (8.7).
//
// 7.4'ün seed'i 5.x/6.x ekranlarını doldurmuştu. Faz 8 dört yeni yüzey
// getirdi ve hiçbiri boş bir veritabanında görünmüyor:
//
//   - PLAN & KOTA (8.5): kotasına dayanmış bir organizasyon olmadan "sınır"
//     yalnızca bir ekran yazısı.
//   - DAVET (8.3): bekleyen bir davet olmadan ekip ekranı boş.
//   - POSTA KUTUSU (8.2): gönderilmiş bir e-posta olmadan ekran boş — ve
//     asıl mesele, o e-postalardaki linklerin GERÇEKTEN çalışması.
//   - ABONELİK (6.4): 8.2'nin posta kutusu geldiğinden beri bu akış da
//     terminal açmadan denenebiliyor.
//
// KURAL DEĞİŞMEDİ (7.4): her şey ham SQL ve TEK TRANSACTION içinde.
//
// # Bu dosyanın en önemli kararı: ÖLÜ LİNK YAZMIYORUZ
//
// Posta kutusuna "gönderilmiş gibi" duran ama tıklanınca 404 veren mesajlar
// yazmak, 8.2'nin bütün amacını (akışın ÇALIŞTIĞINI göstermek) baltalardı.
// Bu yüzden her demo e-postası GERÇEK bir kaydın yanında doğuyor: davet
// e-postasının arkasında gerçek bir davet satırı, abonelik onayının arkasında
// gerçek bir abone satırı var. Token'lar veritabanına 8.3/6.4'ün disiplinine
// göre yazılıyor (onay ve davet HASH'li, iptal HAM) ve e-postaya ham hâli
// gidiyor — yani linkler tıklandığında gerçekten çalışıyor.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/token"
)

const (
	// FreeDemoEmail / FreeDemoPassword, KOTASINA DAYANMIŞ demo hesabı.
	//
	// NEDEN AYRI BİR KULLANICI ve neden aynı kullanıcının ikinci org'u değil:
	// panoda organizasyon değiştirici YOK (MVP dışı) — her ekran
	// `organizations[0]`ı kullanıyor. Aynı kullanıcıya ikinci bir org vermek,
	// o org'u arayüzden ERİŞİLEMEZ kılardı. Ayrı hesap, demo yapan kişinin
	// çıkış yapıp giriş yaparak iki planı da görebilmesi demek.
	FreeDemoEmail    = "free@uptime.local"
	FreeDemoPassword = "demo-uptime-2026"

	freeOrgName = "Startup Deneme"
	freeOrgSlug = "startup"

	// demoInviteEmail, bekleyen davetin adresi (8.3).
	//
	// Adres BİLİNÇLİ OLARAK var olmayan bir hesap: demo yapan kişi posta
	// kutusundaki linke tıklayıp bu adresle KAYDOLARAK akışın tamamını
	// (kaydol → katıl) görebilsin.
	demoInviteEmail = "yeni.uye@example.com"

	// demoSubscriberEmail, ONAYLI abone; demoPendingSubscriberEmail ise
	// onayı BEKLEYEN abone (6.4'ün çift onayı posta kutusundan denensin).
	demoSubscriberEmail        = "abone@example.com"
	demoPendingSubscriberEmail = "yeni.abone@example.com"
)

// freeMonitors, FREE planın kotasını TAM DOLDURAN monitor listesi.
//
// Beşi de 300 saniye ve üstü: demo verisi KENDİ PLANIYLA TUTARLI olmak
// zorunda. Kota yazma anında uygulanıyor (8.5) ve seed ham SQL yazdığı için
// teknik olarak kuralı delebilirdi — ama planına sığmayan bir demo verisi,
// ekranda "5/5 monitor" yazarken kotayı aşan bir organizasyon göstermek
// olurdu. Demo kendi kendisiyle çelişmemeli.
//
// `public` ve `group` ALANLARI BOŞ ve bu bilinçli: bu organizasyonun status
// page'i YAYIMLANMIYOR (5.1'in kuralı gereği `/status/startup` anlamlı bir
// 404 veriyor). Vitrin işini Acme yapıyor; buradaki org'un anlattığı tek şey
// KOTA. Doldurulmuş ama hiçbir yerde okunmayan alanlar, kodu okuyan kişiye
// "bu sayfa yayımlanıyor olmalı" dedirtirdi.
var freeMonitors = []demoMonitor{
	{name: "Pazarlama Sitesi", url: "https://example.com", status: store.MonitorStatusUp, interval: 300},
	{name: "Blog", url: "https://example.org", status: store.MonitorStatusUp, interval: 300},
	{name: "Dokümantasyon", url: "https://example.net", status: store.MonitorStatusUp, interval: 600},
	{name: "Webhook Alıcısı", url: "https://example.com/hooks", status: store.MonitorStatusUp, interval: 900},
	{name: "Yedekleme", url: "https://example.org/backup", status: store.MonitorStatusPending, interval: 3600},
}

// createFreeOrg, kotasına DAYANMIŞ ücretsiz bir organizasyon üretir (8.5).
//
// İNCE TUTULUYOR: 90 günlük geçmiş, kesinti ve bakım penceresi YOK. Bu
// organizasyonun anlattığı tek şey KOTA — vitrin işini Acme yapıyor ve aynı
// zenginliği ikinci kez üretmek seed'i iki katına çıkarır, demoyu
// zenginleştirmezdi.
func (b *builder) createFreeOrg(ctx context.Context) (uuid.UUID, error) {
	hash, err := auth.NewHasher().Hash(FreeDemoPassword)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parola hash'lenemedi: %w", err)
	}

	user, err := b.queries.CreateUser(ctx, store.CreateUserParams{
		Email:        FreeDemoEmail,
		PasswordHash: hash,
		Name:         "Ücretsiz Plan Kullanıcısı",
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("ücretsiz demo kullanıcısı oluşturulamadı: %w", err)
	}

	org, err := b.queries.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: freeOrgName,
		Slug: freeOrgSlug,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("ücretsiz organizasyon oluşturulamadı: %w", err)
	}

	// PLAN AÇIKÇA YAZILMIYOR: şemanın varsayılanı zaten `free` (migration
	// 00021). Elle yazmak, varsayılanın değişmesi hâlinde sessizce ayrışacak
	// ikinci bir kaynak olurdu.

	if _, err := b.queries.AddOrganizationMember(ctx, store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: user.ID,
		Role:   store.OrgRoleOwner,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("ücretsiz org üyeliği eklenemedi: %w", err)
	}

	if _, err := b.tx.Exec(ctx,
		`INSERT INTO status_pages (org_id, slug) VALUES ($1, $2)`, org.ID, freeOrgSlug); err != nil {
		return uuid.Nil, fmt.Errorf("ücretsiz org status page'i oluşturulamadı: %w", err)
	}

	for _, spec := range freeMonitors {
		if _, err := b.createMonitor(ctx, org.ID, spec); err != nil {
			return uuid.Nil, err
		}
	}

	return org.ID, nil
}

// writeInvitation, BEKLEYEN bir davet ve onun e-postasını yazar (8.3).
//
// Token 8.3'ün disiplininde: veritabanında HASH, e-postada HAM. Böylece
// posta kutusundaki link gerçekten çalışıyor — demo yapan kişi tıklayıp
// kaydolarak organizasyona katılabiliyor.
func (b *builder) writeInvitation(ctx context.Context, orgID, invitedBy uuid.UUID) (string, error) {
	raw, err := token.New()
	if err != nil {
		return "", err
	}

	hash := token.Hash(raw)

	// Süre 8.3'ün TTL'iyle aynı mantıkta ileri tarihli: demo verisi süresi
	// dolmuş bir davetle gelmemeli.
	if _, err := b.tx.Exec(ctx, `
		INSERT INTO organization_invitations (org_id, email, role, token_hash, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		orgID, demoInviteEmail, store.OrgRoleAdmin, hash, invitedBy,
		b.now().Add(7*24*time.Hour)); err != nil {
		return "", fmt.Errorf("demo daveti yazılamadı: %w", err)
	}

	return raw, nil
}

// writeSubscribers, biri ONAYLI biri BEKLEYEN iki abone yazar (6.4).
//
// İkisi birden: onaylı abone "bildirim kime gidiyor" sorusunu, bekleyen abone
// ise ÇİFT ONAY akışını gösteriyor — ikincisinin onay linki posta kutusunda
// ve tıklanınca gerçekten çalışıyor.
func (b *builder) writeSubscribers(ctx context.Context, statusPageID uuid.UUID) (subscriberTokens, error) {
	var out subscriberTokens

	unsubscribe, err := token.New()
	if err != nil {
		return out, err
	}
	out.unsubscribe = unsubscribe

	// ONAYLI ABONE: onay token'ı NULL (tek kullanımlık, kullanılmış),
	// iptal token'ı HAM — 6.4'ün ayrımı.
	if _, err := b.tx.Exec(ctx, `
		INSERT INTO status_page_subscribers
			(status_page_id, email, status, unsubscribe_token, confirmed_at, confirm_sent_at)
		VALUES ($1, $2, 'confirmed', $3, $4, $4)`,
		statusPageID, demoSubscriberEmail, unsubscribe, b.now().Add(-48*time.Hour)); err != nil {
		return out, fmt.Errorf("onaylı abone yazılamadı: %w", err)
	}

	confirm, err := token.New()
	if err != nil {
		return out, err
	}
	out.confirm = confirm

	pendingUnsubscribe, err := token.New()
	if err != nil {
		return out, err
	}
	out.pendingUnsubscribe = pendingUnsubscribe

	// BEKLEYEN ABONE: onay token'ı HASH'li saklanıyor, ham hâli e-postada.
	if _, err := b.tx.Exec(ctx, `
		INSERT INTO status_page_subscribers
			(status_page_id, email, status, confirm_token_hash, confirm_sent_at, unsubscribe_token)
		VALUES ($1, $2, 'pending', $3, $4, $5)`,
		statusPageID, demoPendingSubscriberEmail, token.Hash(confirm),
		b.now().Add(-2*time.Hour), pendingUnsubscribe); err != nil {
		return out, fmt.Errorf("bekleyen abone yazılamadı: %w", err)
	}

	return out, nil
}

// subscriberTokens, e-posta gövdelerinde kullanılacak HAM token'lar.
type subscriberTokens struct {
	// confirm, BEKLEYEN abonenin onay token'ı (veritabanında hash'li).
	confirm string
	// unsubscribe, ONAYLI abonenin iptal token'ı (veritabanında ham).
	unsubscribe string
	// pendingUnsubscribe, bekleyen abonenin KENDİ iptal token'ı.
	//
	// Onay e-postasındaki "vazgeç" linki bunu taşıyor — onay token'ını
	// taşısaydı link çalışmazdı (iptal ucu ham iptal token'ı arıyor) ve
	// posta kutusuna ölü bir link yazmış olurduk.
	pendingUnsubscribe string
}

// writeOutbox, geliştirici posta kutusuna GERÇEK linkler taşıyan mesajlar
// yazar (8.2).
//
// `publicBaseURL` argüman: seed hangi adreste çalıştığını bilmiyor ve
// linklerin kökü ARAYÜZÜN adresi (6.4/8.3'ün kararı). Varsayılan
// `http://localhost:3100` — 0.2'de seçilen port.
func (b *builder) writeOutbox(
	ctx context.Context, publicBaseURL, inviteToken string, subs subscriberTokens,
) (int, error) {
	type message struct {
		recipient string
		subject   string
		body      string
		age       time.Duration
	}

	// KONU SATIRLARI 8.3'ün DÜZELTİLMİŞ biçiminde ("Kesinti:" öneki YOK):
	// bunlar işlem e-postası, kesinti bildirimi değil.
	messages := []message{
		{
			recipient: demoInviteEmail,
			subject:   "[" + demoOrgName + "] Organizasyona davet edildiniz",
			body: demoOrgName + " organizasyonuna yönetici rolüyle davet edildiniz.\n" +
				"Katılmak için: " + publicBaseURL + "/invitations/accept?token=" + inviteToken +
				"\n\nBu daveti siz beklemiyorsanız bir şey yapmanıza gerek yok; " +
				"bağlantı 7 gün sonra geçersiz olacak.",
			age: 30 * time.Minute,
		},
		{
			recipient: demoPendingSubscriberEmail,
			subject:   "[" + demoOrgName + "] Aboneliğinizi onaylayın",
			body: demoOrgName + " status sayfasına abone olmak için onaylayın: " +
				publicBaseURL + "/status/" + demoOrgSlug + "/confirm?token=" + subs.confirm +
				"\nVazgeçmek için: " + publicBaseURL + "/status/" + demoOrgSlug +
				"/unsubscribe?token=" + subs.pendingUnsubscribe,
			age: 2 * time.Hour,
		},
		{
			// KESİNTİ BİLDİRİMİ: konu satırı "Kesinti:" önekli, çünkü bu
			// gerçekten bir kesinti bildirimi. İptal linki ONAYLI abonenin
			// HAM token'ıyla — tıklandığında gerçekten aboneliği bitiriyor.
			recipient: demoSubscriberEmail,
			subject:   "[" + demoOrgName + "] Kesinti: Ödeme Servisi yanıt vermiyor",
			body: "Ödeme Servisi yanıt vermiyor.\nEtki: büyük\n\n" +
				"Durumu izleyin: " + publicBaseURL + "/status/" + demoOrgSlug +
				"\nAbonelikten çıkmak için: " + publicBaseURL + "/status/" + demoOrgSlug +
				"/unsubscribe?token=" + subs.unsubscribe,
			age: 20 * time.Hour,
		},
	}

	for _, msg := range messages {
		if _, err := b.tx.Exec(ctx, `
			INSERT INTO email_outbox (recipient, subject, body, created_at)
			VALUES ($1, $2, $3, $4)`,
			msg.recipient, msg.subject, msg.body, b.now().Add(-msg.age)); err != nil {
			return 0, fmt.Errorf("posta kutusu mesajı yazılamadı: %w", err)
		}
	}

	return len(messages), nil
}
