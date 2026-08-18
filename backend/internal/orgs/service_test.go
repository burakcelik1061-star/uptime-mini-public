package orgs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/billing"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu testler GERÇEK Postgres'e karşı koşuyor: slug çakışmasının çözümü
// tamamen UNIQUE kısıtının davranışına dayanıyor (kod "önce bak sonra yaz"
// yapmıyor). Sahte bir store ile ölçülseydi, test kısıtı değil kendi
// taklidini doğrulamış olurdu.

// recordingMailer, gönderilen davet e-postalarını tutan sahte adapter (8.3).
//
// `notifications.Channel`ın ikinci bir uygulaması olması, 6.3'ün
// soyutlamasının gerçekten işe yaradığının kanıtı: davet servisi adapter'ın
// hangisi olduğunu bilmiyor.
type recordingMailer struct {
	targets  []string
	bodies   []string
	failWith error
}

func (m *recordingMailer) Send(_ context.Context, target string, msg notifications.Message) error {
	if m.failWith != nil {
		return m.failWith
	}

	m.targets = append(m.targets, target)
	m.bodies = append(m.bodies, msg.Body)

	return nil
}

func newTestService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()

	return newTestServiceWithMailer(t, pool, &recordingMailer{})
}

func newTestServiceWithMailer(t *testing.T, pool *pgxpool.Pool, mailer Mailer) *Service {
	t.Helper()

	return NewService(pool, store.New(pool), mailer,
		billing.NewProvider(slog.New(slog.NewTextHandler(io.Discard, nil))),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func createUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()

	user, err := store.New(pool).CreateUser(t.Context(), store.CreateUserParams{
		Email:        "uye-" + uuid.NewString() + "@example.com",
		PasswordHash: "$argon2id$test",
		Name:         "Test Kullanıcı",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	return user.ID
}

func TestCreateMakesFounderTheOwner(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)
	userID := createUser(t, pool)

	org, err := svc.Create(t.Context(), userID, "Acme Corp")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if org.Name != "Acme Corp" {
		t.Errorf("ad = %q", org.Name)
	}
	if org.Slug != "acme-corp" {
		t.Errorf("slug = %q, beklenen acme-corp", org.Slug)
	}

	var role store.OrgRole
	row := pool.QueryRow(t.Context(),
		`SELECT role FROM organization_members WHERE org_id = $1 AND user_id = $2`, org.ID, userID)
	if err := row.Scan(&role); err != nil {
		t.Fatalf("üyelik okunamadı: %v", err)
	}
	if role != store.OrgRoleOwner {
		t.Errorf("rol = %q, beklenen owner", role)
	}
}

// Aynı ada sahip organizasyonlar farklı slug almalı — çakışmayı UNIQUE
// kısıtı bildiriyor, kod bir sonraki adayı deniyor.
func TestCreateResolvesSlugCollisions(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)
	userID := createUser(t, pool)

	slugs := map[string]bool{}
	for i := range 6 {
		org, err := svc.Create(t.Context(), userID, "Acme Corp")
		if err != nil {
			t.Fatalf("%d. organizasyon oluşturulamadı: %v", i+1, err)
		}
		if slugs[org.Slug] {
			t.Fatalf("slug tekrar etti: %q", org.Slug)
		}
		slugs[org.Slug] = true
	}

	// İlk ikisi okunabilir olmalı.
	for _, want := range []string{"acme-corp", "acme-corp-2"} {
		if !slugs[want] {
			t.Errorf("beklenen slug üretilmedi: %q (üretilenler: %v)", want, slugs)
		}
	}
}

// Slug'a çevrilemeyen bir ad da kabul edilmeli: veritabanı kısıtı boş slug'ı
// reddederdi, yedek değer bunu önlüyor.
func TestCreateAcceptsNameWithoutSluggableCharacters(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)
	userID := createUser(t, pool)

	org, err := svc.Create(t.Context(), userID, "日本語")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if org.Slug != fallbackSlug {
		t.Errorf("slug = %q, beklenen %q", org.Slug, fallbackSlug)
	}
	if org.Name != "日本語" {
		t.Errorf("ad değişmiş: %q — slug ile ad ayrı şeyler", org.Name)
	}
}

// Organizasyon ile kurucu üyelik AYNI transaction'da yazılıyor: sahipsiz bir
// organizasyon kimsenin erişemediği bir kayıt olurdu.
func TestCreateIsAtomic(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)

	// Var olmayan bir kullanıcı: üyelik satırı foreign key'e takılacak.
	if _, err := svc.Create(t.Context(), uuid.New(), "Yetim Organizasyon"); err == nil {
		t.Fatal("var olmayan kullanıcıyla organizasyon oluşturuldu")
	}

	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM organizations`).Scan(&count); err != nil {
		t.Fatalf("sayım yapılamadı: %v", err)
	}
	if count != 0 {
		t.Errorf("başarısız işlemden %d organizasyon kaldı", count)
	}
}

func TestListForUserReturnsOnlyOwnOrganizationsWithRole(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)

	first := createUser(t, pool)
	second := createUser(t, pool)

	mine, err := svc.Create(t.Context(), first, "Benim Şirketim")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if _, err := svc.Create(t.Context(), second, "Başkasının Şirketi"); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	rows, err := svc.ListForUser(t.Context(), first)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("organizasyon sayısı = %d, beklenen 1 (%+v)", len(rows), rows)
	}
	if rows[0].ID != mine.ID {
		t.Errorf("başka kullanıcının organizasyonu listelendi: %s", rows[0].ID)
	}
	if rows[0].Role != store.OrgRoleOwner {
		t.Errorf("rol = %q, beklenen owner", rows[0].Role)
	}
}

// Bir kullanıcı birden fazla organizasyona üye olabilmeli (0.1 §2).
func TestUserCanBelongToMultipleOrganizations(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)
	userID := createUser(t, pool)

	for _, name := range []string{"Birinci", "İkinci", "Üçüncü"} {
		if _, err := svc.Create(t.Context(), userID, name); err != nil {
			t.Fatalf("%q oluşturulamadı: %v", name, err)
		}
	}

	rows, err := svc.ListForUser(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("organizasyon sayısı = %d, beklenen 3", len(rows))
	}

	// Sıra oluşturma zamanına göre (sorgudaki ORDER BY).
	if rows[0].Name != "Birinci" || rows[2].Name != "Üçüncü" {
		t.Errorf("sıralama beklenenden farklı: %q … %q", rows[0].Name, rows[2].Name)
	}
}

func TestListForUserWithoutOrganizationsIsEmpty(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)

	rows, err := svc.ListForUser(t.Context(), createUser(t, pool))
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("boş beklenirken %d kayıt döndü", len(rows))
	}
}

// --- Üyelik doğrulama (1.4) --------------------------------------------------

func TestMembershipForReturnsRoleAndOrganization(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)
	userID := createUser(t, pool)

	org, err := svc.Create(t.Context(), userID, "Acme Corp")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	membership, err := svc.MembershipFor(t.Context(), org.ID, userID)
	if err != nil {
		t.Fatalf("üyelik okunamadı: %v", err)
	}

	if membership.Role != store.OrgRoleOwner {
		t.Errorf("rol = %q, beklenen owner", membership.Role)
	}
	if membership.OrgID != org.ID || membership.UserID != userID {
		t.Errorf("üyelik = %+v", membership)
	}
	// Organizasyon bilgisi de aynı sorgudan geliyor: handler ikinci bir
	// sorgu yapmasın.
	if membership.OrgName != "Acme Corp" || membership.OrgSlug != "acme-corp" {
		t.Errorf("organizasyon bilgisi eksik: %+v", membership)
	}
}

// İZOLASYONUN ÖZÜ: başka organizasyonun kullanıcısı ile hiç var olmayan
// organizasyon AYNI hatayı almalı. Farklı hatalar, çağıran tarafın bir gün
// ikisini farklı cevaplamasına ve "bu org var ama üye değilsin" bilgisinin
// sızmasına kapı açardı.
func TestMembershipForHidesDifferenceBetweenMissingAndForbidden(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)

	owner := createUser(t, pool)
	outsider := createUser(t, pool)

	org, err := svc.Create(t.Context(), owner, "Gizli Şirket")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	_, notMember := svc.MembershipFor(t.Context(), org.ID, outsider)
	_, noSuchOrg := svc.MembershipFor(t.Context(), uuid.New(), outsider)

	if !errors.Is(notMember, ErrNoAccess) {
		t.Errorf("üye olmayan için hata = %v, beklenen ErrNoAccess", notMember)
	}
	if !errors.Is(noSuchOrg, ErrNoAccess) {
		t.Errorf("olmayan organizasyon için hata = %v, beklenen ErrNoAccess", noSuchOrg)
	}
}

func TestListMembersReturnsEveryoneWithRole(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)

	owner := createUser(t, pool)
	invited := createUser(t, pool)

	org, err := svc.Create(t.Context(), owner, "Acme")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	// İkinci üye (davet akışı MVP dışı, doğrudan ekleniyor).
	if _, err := store.New(pool).AddOrganizationMember(t.Context(), store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: invited,
		Role:   store.OrgRoleMember,
	}); err != nil {
		t.Fatalf("üye eklenemedi: %v", err)
	}

	members, err := svc.ListMembers(t.Context(), org.ID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if len(members) != 2 {
		t.Fatalf("üye sayısı = %d, beklenen 2", len(members))
	}
	if members[0].Role != store.OrgRoleOwner || members[1].Role != store.OrgRoleMember {
		t.Errorf("roller beklenenden farklı: %q, %q", members[0].Role, members[1].Role)
	}
	if members[0].ID != owner || members[1].ID != invited {
		t.Error("üyeler katılma sırasına göre dönmedi")
	}
}

// Üyelik başka organizasyona taşınmıyor: aynı kullanıcı iki organizasyonda
// farklı rollere sahip olabilir.
func TestMembershipIsScopedToOrganization(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)

	userID := createUser(t, pool)
	other := createUser(t, pool)

	mine, err := svc.Create(t.Context(), userID, "Benim")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	theirs, err := svc.Create(t.Context(), other, "Onunki")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	// Diğer organizasyona SADECE member olarak ekleniyor.
	if _, err := store.New(pool).AddOrganizationMember(t.Context(), store.AddOrganizationMemberParams{
		OrgID:  theirs.ID,
		UserID: userID,
		Role:   store.OrgRoleMember,
	}); err != nil {
		t.Fatalf("üye eklenemedi: %v", err)
	}

	asOwner, err := svc.MembershipFor(t.Context(), mine.ID, userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	asMember, err := svc.MembershipFor(t.Context(), theirs.ID, userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if !asOwner.HasAtLeast(store.OrgRoleOwner) {
		t.Error("kendi organizasyonunda owner değil")
	}
	if asMember.HasAtLeast(store.OrgRoleAdmin) {
		t.Error("member rolü admin yetkisi verdi — rol organizasyona özgü olmalı")
	}
}

// Aynı kullanıcı bir organizasyona iki kez üye olamaz (UNIQUE kısıtı).
func TestMembershipIsUniquePerUserAndOrganization(t *testing.T) {
	pool := testdb.Pool(t, "orgs")
	svc := newTestService(t, pool)
	userID := createUser(t, pool)

	org, err := svc.Create(t.Context(), userID, "Acme")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	_, err = store.New(pool).AddOrganizationMember(t.Context(), store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: userID,
		Role:   store.OrgRoleAdmin,
	})
	if err == nil {
		t.Fatal("aynı kullanıcı ikinci kez üye yapıldı")
	}
}
