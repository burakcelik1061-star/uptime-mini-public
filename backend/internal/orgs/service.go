// Package orgs, organizasyonların (workspace) oluşturulmasını ve
// listelenmesini yönetir.
//
// Çok kiracılılığın giriş kapısı burası: Faz 2'den itibaren gelen her kayıt
// (monitor, incident, status page) bir organizasyona ait olacak.
package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/billing"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrSlugUnavailable, benzersiz bir slug üretilemediğinde döner. Pratikte
// ulaşılamayacak bir durum (son denemeler rastgele takılı), ama sessizce
// sonsuz döngüye girmektense hata döndürmek doğru.
var ErrSlugUnavailable = errors.New("organizasyon için benzersiz bir slug üretilemedi")

// ErrNoAccess, organizasyon yoksa VEYA kullanıcı üye değilse döner.
//
// İki durum bilinçli olarak TEK hataya indirgeniyor: ayrı hatalar
// döndürmek, çağıran tarafın bir gün ikisini farklı cevaplamasını ve
// "bu organizasyon var ama sen üye değilsin" bilgisinin sızmasını
// kolaylaştırırdı. Sızıntı ancak fark hiç üretilmezse imkânsız.
var ErrNoAccess = errors.New("organizasyona erişim yok")

// maxSlugCollisionAttempts, aynı ada sahip organizasyonlar için denenecek
// slug sayısı.
const maxSlugCollisionAttempts = 8

// organizationsSlugConstraint / statusPagesSlugConstraint, migration'lardaki
// UNIQUE kısıtların Postgres'in verdiği varsayılan adları.
//
// İKİSİ DE ÇAKIŞMA DÖNGÜSÜNÜ TETİKLİYOR (5.1): organizasyonla birlikte bir
// status page satırı da doğuyor ve onun slug'ı da GLOBAL benzersiz. İki isim
// alanı ayrı olduğu için (bkz. migration 00014) org slug'ı boş olsa bile aynı
// slug'ı taşıyan bir status page bulunabilir — o zaman bir sonraki aday
// deneniyor, tıpkı org slug'ı çakıştığında olduğu gibi.
const (
	organizationsSlugConstraint = "organizations_slug_key"
	statusPagesSlugConstraint   = "status_pages_slug_key"
)

// Beginner, transaction başlatabilen bağlantı havuzu (*pgxpool.Pool).
// (internal/sessions'daki aynı adlı arayüzün eşi: her paket ihtiyacı
// kadarını kendi tanımlıyor.)
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Mailer, davet e-postasını gönderen bağımlılık (8.3).
//
// 6.3'ün `notifications.Channel` arayüzünün ta kendisi (6.4'ün
// `subscribers.Mailer`ıyla aynı biçim): ikinci bir e-posta soyutlaması yazmak,
// gerçek sağlayıcıya geçişi ÜÇ yerde birden yapmak demekti.
type Mailer interface {
	Send(ctx context.Context, target string, msg notifications.Message) error
}

// Service, organizasyon işlemleri.
type Service struct {
	db      Beginner
	queries *store.Queries
	// mailer, davet e-postalarını gönderiyor (8.3). nil olabilir — davet
	// akışını kullanmayan çağıranlar (ör. yalnızca üyelik doğrulayan yollar)
	// bunu geçmek zorunda kalmasın diye değil, TESTLER için: gerçek gönderim
	// istemeyen testler sahte bir adapter veriyor.
	mailer Mailer
	// billing, plan yükseltmesinin ödeme adımı (8.5) — MOCK, tek yapıcıdan
	// seçiliyor (bkz. internal/billing → NewProvider).
	billing billing.Provider
	logger  *slog.Logger
}

// NewService, servisi kurar.
func NewService(
	db Beginner,
	queries *store.Queries,
	mailer Mailer,
	payments billing.Provider,
	logger *slog.Logger,
) *Service {
	return &Service{db: db, queries: queries, mailer: mailer, billing: payments, logger: logger}
}

// Create, yeni bir organizasyon açar ve kurucuyu OWNER olarak üye yapar.
//
// İkisi tek transaction içinde: sahipsiz bir organizasyon, kimsenin
// erişemediği ve silemediği bir kayıt olurdu.
func (s *Service) Create(ctx context.Context, ownerID uuid.UUID, name string) (store.Organization, error) {
	name = strings.TrimSpace(name)
	base := slugify(name)

	// ÇAKIŞMA ÇÖZÜMÜ: "önce SELECT ile bak, boşsa yaz" YAPILMIYOR. İki
	// istek aynı anda geldiğinde ikisi de slug'ı boş görüp ikisi de yazmaya
	// çalışırdı. Son hakem veritabanının UNIQUE kısıtı; kod yalnızca kısıtın
	// verdiği hatayı okuyup bir sonraki adayı deniyor.
	for attempt := range maxSlugCollisionAttempts {
		slug, err := candidateSlug(base, attempt)
		if err != nil {
			return store.Organization{}, err
		}

		org, err := s.createWithSlug(ctx, ownerID, name, slug)
		if err == nil {
			s.logger.Info("organizasyon oluşturuldu",
				slog.String("org_id", org.ID.String()),
				slog.String("slug", org.Slug),
				slog.String("owner_id", ownerID.String()))

			return org, nil
		}

		if postgres.IsUniqueViolation(err, organizationsSlugConstraint) ||
			postgres.IsUniqueViolation(err, statusPagesSlugConstraint) {
			continue
		}

		return store.Organization{}, err
	}

	return store.Organization{}, ErrSlugUnavailable
}

func (s *Service) createWithSlug(ctx context.Context, ownerID uuid.UUID, name, slug string) (store.Organization, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return store.Organization{}, fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	org, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{Name: name, Slug: slug})
	if err != nil {
		// Slug çakışmasını çağıran taraf yorumluyor; sarmalamadan
		// döndürülüyor ki hata tipi korunsun.
		return store.Organization{}, err
	}

	if _, err := q.AddOrganizationMember(ctx, store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: ownerID,
		Role:   store.OrgRoleOwner,
	}); err != nil {
		return store.Organization{}, fmt.Errorf("kurucu üye eklenemedi: %w", err)
	}

	// STATUS PAGE DE AYNI TRANSACTION'DA (5.1): org başına tek sayfa (0.1 §5)
	// bir 1-1 ilişki ve o ilişki ancak organizasyonla birlikte doğarsa gerçekten
	// 1-1 kalıyor. "Gerektiğinde oluştur" deseni, sayfayı soran her kod yolunda
	// ikinci bir yazma ihtimali demekti (ve eşzamanlı iki istek iki satır
	// denerdi). Sayfa YAYIMLANMAMIŞ doğuyor: organizasyon açmak, servisleri
	// yayımlamak anlamına gelmemeli.
	//
	// Slug organizasyonunkinden KOPYALANIYOR ama ayrı bir sütun: org'un adı
	// değiştiğinde public status page adresi değişmemeli (bkz. migration 00014).
	// Hata sarmalanmadan dönüyor ki çağıran çakışmayı tanıyıp yeni aday
	// deneyebilsin.
	if _, err := q.CreateStatusPage(ctx, store.CreateStatusPageParams{
		OrgID: org.ID,
		Slug:  org.Slug,
	}); err != nil {
		return store.Organization{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return store.Organization{}, fmt.Errorf("organizasyon kaydedilemedi: %w", err)
	}

	return org, nil
}

// MembershipFor, kullanıcının organizasyondaki üyeliğini döner.
//
// Org-scoped yetkilendirmenin tek veri kaynağı bu: token'da rol/organizasyon
// TAŞINMIYOR. Taşınsaydı, üyelikten çıkarılan bir kullanıcı token'ının ömrü
// (15 dk) boyunca erişmeye devam ederdi.
//
// Organizasyon yoksa da, kullanıcı üye değilse de ErrNoAccess döner.
func (s *Service) MembershipFor(ctx context.Context, orgID, userID uuid.UUID) (Membership, error) {
	row, err := s.queries.GetOrganizationForMember(ctx, store.GetOrganizationForMemberParams{
		ID:     orgID,
		UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Membership{}, ErrNoAccess
		}

		return Membership{}, fmt.Errorf("üyelik okunamadı: %w", err)
	}

	return Membership{
		OrgID:    row.ID,
		UserID:   userID,
		Role:     row.Role,
		OrgName:  row.Name,
		OrgSlug:  row.Slug,
		OrgPlan:  row.Plan,
		OrgSince: row.CreatedAt,
	}, nil
}

// ListMembers, organizasyonun üyelerini döner.
//
// Erişim kontrolü ÇAĞIRANIN işi (middleware zaten üyeliği doğruladı); bu
// katman "kim sorabilir" sorusunu değil "cevap ne" sorusunu cevaplıyor.
func (s *Service) ListMembers(ctx context.Context, orgID uuid.UUID) ([]store.ListOrganizationMembersRow, error) {
	rows, err := s.queries.ListOrganizationMembers(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("üyeler okunamadı: %w", err)
	}

	return rows, nil
}

// ListForUser, kullanıcının üyesi olduğu organizasyonları rolüyle döner.
func (s *Service) ListForUser(ctx context.Context, userID uuid.UUID) ([]store.ListOrganizationsForUserRow, error) {
	rows, err := s.queries.ListOrganizationsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("organizasyonlar okunamadı: %w", err)
	}

	return rows, nil
}
