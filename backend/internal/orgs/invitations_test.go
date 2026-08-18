package orgs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/token"
)

// DAVET AKIŞI (8.3) — gerçek veritabanına karşı.
//
// Neden gerçek DB: bu akışın iki garantisi tamamen ŞEMADA duruyor — kısmi
// unique indeks ("aynı adrese ikinci bekleyen davet olmasın") ve kabul
// transaction'ı. Sahte bir store ile ölçülseydi test kısıtı değil kendi
// taklidini doğrulamış olurdu (1.2'den beri paketin kuralı).
//
// `testdb.Pool` HER ÇAĞRIDA TRUNCATE ediyor: fixture havuzu bir kez alıp
// taşıyor, testler ikinci kez çağırmıyor.

type inviteFixture struct {
	pool   *pgxpool.Pool
	svc    *Service
	mailer *recordingMailer
	owner  Membership
	orgID  uuid.UUID
}

func newInviteFixture(t *testing.T) inviteFixture {
	t.Helper()

	pool := testdb.Pool(t, "orgs")
	mailer := &recordingMailer{}
	svc := newTestServiceWithMailer(t, pool, mailer)

	ownerID := createUser(t, pool)
	org, err := svc.Create(t.Context(), ownerID, "Acme Corp")
	if err != nil {
		t.Fatalf("organizasyon oluşturulamadı: %v", err)
	}

	membership, err := svc.MembershipFor(t.Context(), org.ID, ownerID)
	if err != nil {
		t.Fatalf("üyelik okunamadı: %v", err)
	}

	return inviteFixture{pool: pool, svc: svc, mailer: mailer, owner: membership, orgID: org.ID}
}

// invite, daveti gönderir ve e-postaya düşen HAM token'ı döner.
//
// Token'ı testin içinden üretmek yerine e-postadan okumak bilinçli: ölçülen
// şey "servis ne yazdı" değil, DAVET EDİLEN KİŞİNİN ELİNE NE GEÇTİĞİ.
func (f inviteFixture) invite(t *testing.T, email string, role store.OrgRole) string {
	t.Helper()

	if err := f.svc.Invite(t.Context(), f.owner, email, role, "http://localhost:3100"); err != nil {
		t.Fatalf("davet gönderilemedi: %v", err)
	}

	body := f.mailer.bodies[len(f.mailer.bodies)-1]
	_, after, found := strings.Cut(body, "token=")
	if !found {
		t.Fatalf("davet gövdesinde token yok: %q", body)
	}

	return strings.Fields(after)[0]
}

// createUserWithEmail, belirli bir adresle kullanıcı açar.
func createUserWithEmail(t *testing.T, svc *Service, email string) uuid.UUID {
	t.Helper()

	user, err := svc.queries.CreateUser(t.Context(), store.CreateUserParams{
		Email:        email,
		PasswordHash: "$argon2id$test",
		Name:         "Davetli",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	return user.ID
}

func TestInviteSendsLinkAndStoresHashedToken(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "Yeni@Example.COM", store.OrgRoleMember)

	if len(f.mailer.targets) != 1 {
		t.Fatalf("gönderilen e-posta sayısı = %d, beklenen 1", len(f.mailer.targets))
	}

	// ADRES NORMALLEŞTİRİLİYOR (1.1'in kuralı): davet "Yeni@Example.COM"a
	// yazıldıysa "yeni@example.com" ile giren kişi kabul edebilmeli.
	if f.mailer.targets[0] != "yeni@example.com" {
		t.Errorf("alıcı = %q, normalleştirilmedi", f.mailer.targets[0])
	}

	// TOKEN VERİTABANINDA HAM DURMUYOR (1.2/6.4'ün kararı): hash'i eşleşiyor,
	// ham hâli eşleşmiyor.
	hash := token.Hash(raw)
	if _, err := f.svc.queries.GetInvitationByTokenHash(t.Context(), &hash); err != nil {
		t.Fatalf("hash'li token ile davet bulunamadı: %v", err)
	}
	if _, err := f.svc.queries.GetInvitationByTokenHash(t.Context(), &raw); err == nil {
		t.Fatal("HAM token ile davet bulundu — token hash'lenmeden saklanıyor")
	}
}

// YETKİ YÜKSELTME KAPISI — bu adımın en önemli testi.
//
// Bir admin kendine owner davet edip ikinci bir hesapla organizasyonu
// devralabilseydi, 1.4'ün rol sıralaması kâğıt üstünde kalırdı.
func TestInviteRejectsRoleAboveInviterOwn(t *testing.T) {
	f := newInviteFixture(t)

	admin := f.owner
	admin.Role = store.OrgRoleAdmin

	if err := f.svc.Invite(
		t.Context(), admin, "yukari@example.com", store.OrgRoleOwner, "http://localhost:3100",
	); !errors.Is(err, ErrRoleTooHigh) {
		t.Fatalf("hata = %v, beklenen ErrRoleTooHigh", err)
	}

	// Reddedilen davet E-POSTA DA ÜRETMEMELİ.
	if len(f.mailer.targets) != 0 {
		t.Errorf("reddedilen davet için e-posta gönderildi: %v", f.mailer.targets)
	}

	// Admin KENDİ seviyesini ve altını davet edebiliyor.
	for _, role := range []store.OrgRole{store.OrgRoleAdmin, store.OrgRoleMember} {
		if err := f.svc.Invite(
			t.Context(), admin, string(role)+"@example.com", role, "http://localhost:3100",
		); err != nil {
			t.Errorf("admin %q davet edemedi: %v", role, err)
		}
	}
}

// ENUMERATION: aynı adrese ikinci davet AYNI cevabı alıyor ama İKİNCİ BİR
// E-POSTA GÖNDERİLMİYOR (6.4'ün "zaten onaylı aboneye yeni onay yok" kararı).
func TestInviteIsIdempotentAndSilentForPendingAddress(t *testing.T) {
	f := newInviteFixture(t)

	for range 3 {
		if err := f.svc.Invite(
			t.Context(), f.owner, "ayni@example.com", store.OrgRoleMember, "http://localhost:3100",
		); err != nil {
			t.Fatalf("davet reddedildi: %v", err)
		}
	}

	if len(f.mailer.targets) != 1 {
		t.Errorf("gönderilen e-posta sayısı = %d, beklenen 1 — tekrar eden davet posta üretmemeli",
			len(f.mailer.targets))
	}

	rows, err := f.svc.queries.ListPendingInvitations(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("davetler okunamadı: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("bekleyen davet sayısı = %d, beklenen 1 (kısmi unique indeks)", len(rows))
	}
}

func TestInviteRejectsMalformedEmail(t *testing.T) {
	f := newInviteFixture(t)

	for _, email := range []string{"", "duz-metin", "ali@localhost", "a@b."} {
		if err := f.svc.Invite(
			t.Context(), f.owner, email, store.OrgRoleMember, "http://localhost:3100",
		); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%q için hata = %v, beklenen ErrInvalidEmail", email, err)
		}
	}
}

func TestAcceptInvitationAddsMemberWithInvitedRole(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "davetli@example.com", store.OrgRoleAdmin)
	invitee := createUserWithEmail(t, f.svc, "davetli@example.com")

	accepted, err := f.svc.AcceptInvitation(t.Context(), invitee, "davetli@example.com", raw)
	if err != nil {
		t.Fatalf("davet kabul edilemedi: %v", err)
	}

	if accepted.OrgID != f.orgID {
		t.Errorf("org = %s, beklenen %s", accepted.OrgID, f.orgID)
	}
	if accepted.OrgName != "Acme Corp" {
		t.Errorf("org adı = %q", accepted.OrgName)
	}
	if accepted.Role != store.OrgRoleAdmin {
		t.Errorf("rol = %q, beklenen admin", accepted.Role)
	}

	// Üyelik GERÇEKTEN yazıldı ve 1.4'ün sorgusundan geçiyor.
	membership, err := f.svc.MembershipFor(t.Context(), f.orgID, invitee)
	if err != nil {
		t.Fatalf("davet kabul edildi ama üyelik yok: %v", err)
	}
	if membership.Role != store.OrgRoleAdmin {
		t.Errorf("üyelik rolü = %q", membership.Role)
	}
}

// TEK KULLANIMLIK: aynı link ikinci kez çalışmamalı.
func TestAcceptInvitationIsSingleUse(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "tek@example.com", store.OrgRoleMember)
	invitee := createUserWithEmail(t, f.svc, "tek@example.com")

	if _, err := f.svc.AcceptInvitation(t.Context(), invitee, "tek@example.com", raw); err != nil {
		t.Fatalf("ilk kabul başarısız: %v", err)
	}

	_, err := f.svc.AcceptInvitation(t.Context(), invitee, "tek@example.com", raw)
	if !errors.Is(err, ErrInvitationNotFound) {
		t.Fatalf("ikinci kabul hatası = %v, beklenen ErrInvitationNotFound", err)
	}
}

// ADRES EŞLEŞMESİ: sızan bir link başkasının elinde işe yaramamalı.
func TestAcceptInvitationRejectsDifferentEmail(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "dogru@example.com", store.OrgRoleMember)
	stranger := createUserWithEmail(t, f.svc, "yabanci@example.com")

	if _, err := f.svc.AcceptInvitation(
		t.Context(), stranger, "yabanci@example.com", raw,
	); !errors.Is(err, ErrInvitationEmailMismatch) {
		t.Fatalf("hata = %v, beklenen ErrInvitationEmailMismatch", err)
	}

	// Reddedilen kabul ÜYELİK YAZMAMALI.
	if _, err := f.svc.MembershipFor(t.Context(), f.orgID, stranger); !errors.Is(err, ErrNoAccess) {
		t.Error("eşleşmeyen adresle üyelik yazıldı")
	}

	// Ve daveti TÜKETMEMELİ: meşru sahibi hâlâ kullanabilmeli.
	invitee := createUserWithEmail(t, f.svc, "dogru@example.com")
	if _, err := f.svc.AcceptInvitation(t.Context(), invitee, "dogru@example.com", raw); err != nil {
		t.Fatalf("meşru kabul, reddedilen denemeden sonra çalışmadı: %v", err)
	}
}

// SÜRE DOLMASI ayrı bir cevap alıyor: kullanıcının yapacağı iş farklı
// ("yöneticiden yenisini iste" vs. "linkin yanlış").
//
// Saat sahtelenmiyor (`clock` arayüzü bu tek test için taşınacak bir
// soyutlama olurdu); satırın süresi doğrudan GEÇMİŞE çekiliyor — 7.4'ün seed
// testlerinin geçmişe tarihli veri yazarken kullandığı yaklaşımın aynısı.
func TestAcceptInvitationRejectsExpiredToken(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "gec@example.com", store.OrgRoleMember)
	invitee := createUserWithEmail(t, f.svc, "gec@example.com")

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE organization_invitations SET expires_at = now() - interval '1 hour'
		 WHERE org_id = $1`, f.orgID); err != nil {
		t.Fatalf("süre geçmişe çekilemedi: %v", err)
	}

	_, err := f.svc.AcceptInvitation(t.Context(), invitee, "gec@example.com", raw)
	if !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("hata = %v, beklenen ErrInvitationExpired", err)
	}

	// Süresi dolmuş davet ÜYELİK YAZMAMALI.
	if _, err := f.svc.MembershipFor(t.Context(), f.orgID, invitee); !errors.Is(err, ErrNoAccess) {
		t.Error("süresi dolmuş davetle üyelik yazıldı")
	}
}

func TestAcceptInvitationRejectsUnknownToken(t *testing.T) {
	f := newInviteFixture(t)

	invitee := createUserWithEmail(t, f.svc, "kimse@example.com")

	_, err := f.svc.AcceptInvitation(t.Context(), invitee, "kimse@example.com", "uydurma-token")
	if !errors.Is(err, ErrInvitationNotFound) {
		t.Fatalf("hata = %v, beklenen ErrInvitationNotFound", err)
	}
}

// ZATEN ÜYE OLAN BİRİNİN ROLÜ DEĞİŞMİYOR: davet gönderildikten sonra bir
// admin o kişiyi elle başka bir rolle eklemiş olabilir ve linke tıklamak, o
// DAHA YENİ yönetim kararını sessizce ezmek olurdu.
func TestAcceptInvitationKeepsExistingRole(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "zaten@example.com", store.OrgRoleAdmin)
	invitee := createUserWithEmail(t, f.svc, "zaten@example.com")

	// Davet beklerken elle `member` olarak ekleniyor.
	if _, err := f.svc.queries.AddOrganizationMember(t.Context(), store.AddOrganizationMemberParams{
		OrgID:  f.orgID,
		UserID: invitee,
		Role:   store.OrgRoleMember,
	}); err != nil {
		t.Fatalf("üye eklenemedi: %v", err)
	}

	if _, err := f.svc.AcceptInvitation(t.Context(), invitee, "zaten@example.com", raw); err != nil {
		t.Fatalf("davet kabul edilemedi: %v", err)
	}

	membership, err := f.svc.MembershipFor(t.Context(), f.orgID, invitee)
	if err != nil {
		t.Fatalf("üyelik okunamadı: %v", err)
	}
	if membership.Role != store.OrgRoleMember {
		t.Errorf("rol = %q, beklenen member — davet mevcut rolü ezmemeli", membership.Role)
	}

	// Davet yine de TÜKETİLDİ: link ikinci kez çalışmamalı.
	if _, err := f.svc.AcceptInvitation(
		t.Context(), invitee, "zaten@example.com", raw,
	); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("davet tüketilmedi: %v", err)
	}
}

func TestRevokeInvitationRemovesPendingInvite(t *testing.T) {
	f := newInviteFixture(t)

	raw := f.invite(t, "iptal@example.com", store.OrgRoleMember)

	rows, err := f.svc.ListInvitations(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("davetler okunamadı: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("bekleyen davet sayısı = %d", len(rows))
	}

	if err := f.svc.RevokeInvitation(t.Context(), f.orgID, rows[0].ID); err != nil {
		t.Fatalf("davet iptal edilemedi: %v", err)
	}

	// İptal edilen davetin LİNKİ DE ÖLMELİ.
	invitee := createUserWithEmail(t, f.svc, "iptal@example.com")
	if _, err := f.svc.AcceptInvitation(
		t.Context(), invitee, "iptal@example.com", raw,
	); !errors.Is(err, ErrInvitationNotFound) {
		t.Errorf("iptal edilen davet hâlâ kabul edilebiliyor: %v", err)
	}
}

// Yabancı bir organizasyonun daveti iptal edilemiyor: "yok" ile "başkasının"
// aynı cevap (1.4'ün disiplini).
func TestRevokeInvitationIsScopedToOrganization(t *testing.T) {
	f := newInviteFixture(t)

	f.invite(t, "baska@example.com", store.OrgRoleMember)

	rows, err := f.svc.ListInvitations(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("davetler okunamadı: %v", err)
	}

	if err := f.svc.RevokeInvitation(t.Context(), uuid.New(), rows[0].ID); !errors.Is(err, ErrInvitationNotFound) {
		t.Fatalf("yabancı org daveti iptal etti: %v", err)
	}
}

func TestListInvitationsReturnsInviterAndExpiry(t *testing.T) {
	f := newInviteFixture(t)

	before := time.Now()
	f.invite(t, "liste@example.com", store.OrgRoleAdmin)

	rows, err := f.svc.ListInvitations(t.Context(), f.orgID)
	if err != nil {
		t.Fatalf("davetler okunamadı: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("davet sayısı = %d", len(rows))
	}

	row := rows[0]
	if row.Email != "liste@example.com" {
		t.Errorf("adres = %q", row.Email)
	}
	if row.Role != store.OrgRoleAdmin {
		t.Errorf("rol = %q", row.Role)
	}
	if !row.ExpiresAt.After(before) {
		t.Errorf("süre geçmişte: %s", row.ExpiresAt)
	}
	if row.InvitedByEmail == nil {
		t.Error("davet edenin adresi dönmedi")
	}
}

// ParseRole tanınmayan değeri sessizce `member`a düşürmemeli (1.4'ün
// "tanımıyorum ile izin ver aynı şey değil" kuralı).
func TestParseRoleRejectsUnknownValue(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "OWNER", "superadmin", "root"} {
		if _, err := ParseRole(raw); !errors.Is(err, ErrInvalidRole) {
			t.Errorf("%q kabul edildi", raw)
		}
	}

	for _, raw := range []string{"owner", "admin", "member"} {
		if _, err := ParseRole(raw); err != nil {
			t.Errorf("%q reddedildi: %v", raw, err)
		}
	}
}
