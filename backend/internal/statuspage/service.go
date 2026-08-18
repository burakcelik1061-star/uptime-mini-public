// Package statuspage, organizasyonun public status sayfasını yönetir (Faz 5.1).
//
// Paketin İKİ YÜZÜ var ve ayrım bu adımın tamamını belirliyor:
//
//   - ORG-SCOPED (service.go) — admin tarafı: sayfayı oku, yayımla, hangi
//     servislerin public olduğunu seç. Çağıranı `requireOrgMembership`
//     ağacındaki bir handler, yani kimlik ve organizasyon zaten doğrulanmış.
//
//   - PUBLIC (public.go) — ziyaretçi tarafı: OTURUMSUZ. Ne token var ne üyelik;
//     organizasyonu yalnızca SLUG belirliyor. Bu yüzden izolasyon buradaki
//     hiçbir middleware'e emanet edilmiyor, sorguların içinde yazılı
//     (db/queries/status_pages.sql).
//
// İkisinin aynı pakette olmasının sebebi, ikisinin de aynı tek gerçeğe bağlı
// olması: "yayımlanan servis" = `status_page_items` satırı. İki ayrı paket
// olsaydı bu kural iki yerde tekrarlanırdı ve sızıntı tam olarak o
// tekrarların ayrıştığı gün doğardı.
package statuspage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

const (
	// MaxItems, bir status page'de yayımlanabilecek servis sayısı.
	//
	// Sunucunun kendini koruması (incidents.MaxMonitorsPerIncident ile aynı
	// gerekçe): seçim ucu bir listeyi tümden değiştirdiği için gövde sınırsız
	// büyüyebilirdi.
	MaxItems = 200

	// MaxGroups / MaxGroupNameLength, gruplamanın sınırları (5.3).
	// Grup adı sınırı şemadaki CHECK ile aynı.
	MaxGroups          = 20
	MaxGroupNameLength = 60
)

// ErrNotFound, organizasyonun status page'i yoksa VEYA aranan slug için
// yayımlanmış bir sayfa yoksa döner.
//
// İki durum TEK hataya indirgeniyor (1.4'ün `orgs.ErrNoAccess` disiplini):
// "bu slug var ama yayımda değil" bilgisi üretilmemeli, yoksa denenen
// slug'lardan hangi organizasyonların kayıtlı olduğu dışarıdan öğrenilebilirdi.
var ErrNotFound = errors.New("status page bulunamadı")

// Problem / ValidationError, alan bazlı doğrulama sonucu (incidents ve
// monitors paketlerindeki ikizlerinden bilinçli olarak bağımsız: hiçbiri
// HTTP'yi tanımıyor, çeviriyi handler yapıyor).
type Problem struct {
	Field   string
	Message string
}

// ValidationError, kabul edilemez alanları taşır.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Field+": "+p.Message)
	}

	return "status page ayarı doğrulanamadı (" + strings.Join(parts, ", ") + ")"
}

// Beginner, transaction başlatabilen bağlantı havuzu (*pgxpool.Pool).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Service, status page işlemleri.
type Service struct {
	db      Beginner
	queries *store.Queries
	logger  *slog.Logger
}

// NewService, servisi kurar.
func NewService(db Beginner, queries *store.Queries, logger *slog.Logger) *Service {
	return &Service{db: db, queries: queries, logger: logger}
}

// Item, yayımlanan bir servisin ADMİN gözüyle hâli.
//
// Public karşılığından (PublicService) fazlası var: `URL`. Ziyaretçi izlenen
// adresi görmüyor, admin ekranı ise hangi monitor'ün seçildiğini adres olmadan
// ayırt edemeyebilir (aynı adlı iki monitor).
type Item struct {
	MonitorID       uuid.UUID
	Name            string
	URL             string
	Status          store.MonitorStatus
	StatusChangedAt time.Time
	// Group, servisin grubu (5.3); nil = gruplanmamış.
	Group *string
}

// Settings, admin ekranının gördüğü hâl: sayfa kaydı + grupları + yayımlanan
// servisler.
type Settings struct {
	Page store.StatusPage
	// Groups, kullanıcının belirlediği SIRADA (5.3).
	Groups []string
	Items  []Item
}

// ServicesInput, yayımlanan servis seçiminin TAMAMI (5.1'in seçimi + 5.3'ün
// gruplaması).
//
// İkisi TEK gövdede, çünkü tek transaction'da yazılmak zorundalar: gruplar
// ayrı bir uçtan yazılsaydı iki başarısızlık biçimi doğardı — servisleri
// olmayan bir grup ve silinmiş bir gruba işaret eden servis.
type ServicesInput struct {
	// Groups, grup adları — SIRA ÖNEMLİ (ekrandaki liste sırası).
	Groups []string
	Items  []ItemInput
}

// ItemInput, yayımlanacak tek bir servis.
type ItemInput struct {
	MonitorID uuid.UUID
	// Group, `Groups` içinde geçen bir ad ya da boş dize (gruplanmamış).
	// Kimlik değil AD: gruplar her kayıtta yeniden yazıldığı için kimlikleri
	// de değişiyor (bkz. db/queries/status_pages.sql).
	Group string
}

// Get, organizasyonun status page ayarlarını döndürür.
//
// Sayfa HER ZAMAN var (1-1 ilişki organizasyon açılışında kuruluyor, bkz.
// migration 00014 ve internal/orgs). ErrNotFound yine de mümkün: eski bir
// organizasyon elle temizlenmişse çağıran 404 görsün, sunucu 500 vermesin.
func (s *Service) Get(ctx context.Context, orgID uuid.UUID) (Settings, error) {
	page, err := s.queries.GetStatusPageForOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNotFound
		}

		return Settings{}, fmt.Errorf("status page okunamadı: %w", err)
	}

	return s.settingsFor(ctx, s.queries, orgID, page)
}

// UpdateSettings, sayfanın ayarlarını kısmen günceller (5.1'in yayım bayrağı +
// 5.3'ün görünüm alanları).
//
// TEK UÇ: yayımlama ayrı bir uçta durabilirdi ama hepsi aynı kaydın alanı ve
// iki ayrı yazma yolu, "yayımla" ile "başlığı değiştir"in birbirini ezmesine
// açık olurdu. Birleştirme burada (oku → bindir → BÜTÜNÜ doğrula → yaz):
// 2.1'in PATCH kararının aynısı.
func (s *Service) UpdateSettings(ctx context.Context, orgID uuid.UUID, patch SettingsPatch) (Settings, error) {
	if patch.IsEmpty() {
		return Settings{}, &ValidationError{Problems: []Problem{{
			Field:   "body",
			Message: "en az bir alan gönderilmeli",
		}}}
	}

	current, err := s.queries.GetStatusPageForOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNotFound
		}

		return Settings{}, fmt.Errorf("status page okunamadı: %w", err)
	}

	values := patch.apply(settingsValues{
		IsPublished: current.IsPublished,
		Title:       current.Title,
		Description: current.Description,
		LogoURL:     current.LogoUrl,
		ThemeColor:  current.ThemeColor,
	}).normalize()

	if err := values.validate(); err != nil {
		return Settings{}, err
	}

	page, err := s.queries.UpdateStatusPageSettings(ctx, store.UpdateStatusPageSettingsParams{
		OrgID:       orgID,
		IsPublished: values.IsPublished,
		Title:       values.Title,
		Description: values.Description,
		LogoUrl:     values.LogoURL,
		ThemeColor:  values.ThemeColor,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNotFound
		}

		return Settings{}, fmt.Errorf("status page ayarları kaydedilemedi: %w", err)
	}

	s.logger.Info("status page ayarları güncellendi",
		slog.String("org_id", orgID.String()),
		slog.String("slug", page.Slug),
		slog.Bool("is_published", page.IsPublished))

	return s.settingsFor(ctx, s.queries, orgID, page)
}

// ReplaceServices, yayımlanan servis seçimini VE gruplamayı TÜMDEN değiştirir.
//
// "Ekle" ve "çıkar" yerine "liste bu olsun": iki ayrı uç, arayüzdeki seçim ile
// sunucudaki seçimin sessizce ayrışmasına açıktı ve o ayrışmanın adı
// "yayımlanmaması gereken bir servis yayımda kaldı".
//
// GRUPLAR DA AYNI YAZMADA (5.3): ayrı bir uçtan yazılsalardı iki başarısızlık
// biçimi doğardı — servisleri olmayan bir grup ve silinmiş bir gruba işaret
// eden servis.
//
// HEPSİ TEK TRANSACTION'DA. Ayrı ayrı yapılsaydı aradaki bir hata sayfayı BOŞ
// bırakırdı — ziyaretçi için "hiçbir servisimiz yok" demek. Yazılan satır
// sayısı istenenden azsa (listede yabancı ya da olmayan bir kimlik var) işlem
// tümden geri alınıyor: 4.3/4.4'ün aynı deseni.
func (s *Service) ReplaceServices(ctx context.Context, orgID uuid.UUID, in ServicesInput) (Settings, error) {
	in = NormalizeServices(in)
	if err := ValidateServices(in); err != nil {
		return Settings{}, err
	}

	page, err := s.queries.GetStatusPageForOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{}, ErrNotFound
		}

		return Settings{}, fmt.Errorf("status page okunamadı: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := s.queries.WithTx(tx)

	// Sıra önemli: gruplar ÖNCE yazılıyor, çünkü servis satırları onlara ada
	// göre bağlanıyor.
	if err := queries.DeleteStatusPageGroups(ctx, page.ID); err != nil {
		return Settings{}, fmt.Errorf("gruplar temizlenemedi: %w", err)
	}

	for position, name := range in.Groups {
		if _, err := queries.CreateStatusPageGroup(ctx, store.CreateStatusPageGroupParams{
			StatusPageID: page.ID,
			Name:         name,
			Position:     int32(position), //nolint:gosec // sınırı ValidateServices koyuyor
		}); err != nil {
			return Settings{}, fmt.Errorf("grup oluşturulamadı: %w", err)
		}
	}

	if err := queries.DeleteStatusPageItems(ctx, page.ID); err != nil {
		return Settings{}, fmt.Errorf("yayımlanan servisler temizlenemedi: %w", err)
	}

	if len(in.Items) > 0 {
		monitorIDs := make([]uuid.UUID, 0, len(in.Items))
		groupNames := make([]string, 0, len(in.Items))

		for _, item := range in.Items {
			monitorIDs = append(monitorIDs, item.MonitorID)
			groupNames = append(groupNames, item.Group)
		}

		added, err := queries.AddStatusPageItems(ctx, store.AddStatusPageItemsParams{
			StatusPageID: page.ID,
			MonitorIds:   monitorIDs,
			GroupNames:   groupNames,
			OrgID:        orgID,
		})
		if err != nil {
			return Settings{}, fmt.Errorf("servisler yayımlanamadı: %w", err)
		}

		if int(added) != len(in.Items) {
			return Settings{}, &ValidationError{Problems: []Problem{{
				Field:   "items",
				Message: "bu organizasyona ait olmayan veya bulunmayan servis var",
			}}}
		}
	}

	settings, err := s.settingsFor(ctx, queries, orgID, page)
	if err != nil {
		return Settings{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Settings{}, fmt.Errorf("yayımlanan servisler kaydedilemedi: %w", err)
	}

	s.logger.Info("status page servis seçimi güncellendi",
		slog.String("org_id", orgID.String()),
		slog.String("slug", page.Slug),
		slog.Int("services", len(settings.Items)),
		slog.Int("groups", len(settings.Groups)))

	return settings, nil
}

// settingsFor, sayfa kaydının yanına grupları ve servisleri okur. `queries`
// parametre: aynı okuma hem havuzdan hem transaction içinden çağrılıyor.
func (s *Service) settingsFor(
	ctx context.Context, queries *store.Queries, orgID uuid.UUID, page store.StatusPage,
) (Settings, error) {
	groupRows, err := queries.ListStatusPageGroupsForOrg(ctx, orgID)
	if err != nil {
		return Settings{}, fmt.Errorf("gruplar okunamadı: %w", err)
	}

	groups := make([]string, 0, len(groupRows))
	for _, row := range groupRows {
		groups = append(groups, row.Name)
	}

	rows, err := queries.ListStatusPageItemsForOrg(ctx, orgID)
	if err != nil {
		return Settings{}, fmt.Errorf("yayımlanan servisler okunamadı: %w", err)
	}

	items := make([]Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, Item{
			MonitorID:       row.MonitorID,
			Name:            row.Name,
			URL:             row.Url,
			Status:          row.CurrentStatus,
			StatusChangedAt: row.StatusChangedAt,
			Group:           row.GroupName,
		})
	}

	return Settings{Page: page, Groups: groups, Items: items}, nil
}

// NormalizeServices, girdiyi saklama biçimine getirir.
//
// Doğrulama HER ZAMAN normalleştirilmiş değeri görmeli (2.1'in kuralı): yoksa
// yalnızca boşluktan oluşan bir grup adı kuralı geçip veritabanına giderdi.
func NormalizeServices(in ServicesInput) ServicesInput {
	groups := make([]string, 0, len(in.Groups))
	for _, name := range in.Groups {
		groups = append(groups, strings.TrimSpace(name))
	}

	items := make([]ItemInput, 0, len(in.Items))
	for _, item := range in.Items {
		item.Group = strings.TrimSpace(item.Group)
		items = append(items, item)
	}

	in.Groups = groups
	in.Items = items

	return in
}

// ValidateServices, seçilen servisleri ve gruplarını denetler.
//
// BOŞ LİSTE GEÇERLİ: "hiçbir servisi yayımlamıyorum" meşru bir durum (sayfa
// yayımdan kaldırılmadan servisler geri çekilebilmeli). İncident'ın aksine —
// orada boş liste "hangi servis etkilendi" sorusunu cevapsız bırakırdı.
//
// Dışa açık: HTTP katmanının testleri kuralların kendisini değil, 422'ye
// çevrimini ölçmeli (incidents/monitors ile aynı gerekçe).
func ValidateServices(in ServicesInput) error {
	problems := validateGroups(in.Groups)

	if len(in.Items) > MaxItems {
		problems = append(problems, Problem{
			Field:   "items",
			Message: fmt.Sprintf("en fazla %d servis yayımlanabilir", MaxItems),
		})
	}

	known := make(map[string]struct{}, len(in.Groups))
	for _, name := range in.Groups {
		known[name] = struct{}{}
	}

	seen := make(map[uuid.UUID]struct{}, len(in.Items))

	for _, item := range in.Items {
		if _, dup := seen[item.MonitorID]; dup {
			// Sessizce tekilleştirmek yerine hata: yinelenen kimlik gönderen
			// istemci, gönderdiğinden farklı bir listenin kaydedildiğini
			// bilmeli (ve satır sayısı karşılaştırması da yanlış alarm verirdi).
			problems = append(problems, Problem{
				Field:   "items",
				Message: "aynı servis birden fazla kez gönderilemez",
			})

			break
		}

		seen[item.MonitorID] = struct{}{}
	}

	for _, item := range in.Items {
		if item.Group == "" {
			continue
		}

		if _, ok := known[item.Group]; !ok {
			// Tanınmayan grup adı sessizce "gruplanmamış"a DÜŞMÜYOR: sorgudaki
			// LEFT JOIN öyle davranırdı ve kullanıcı seçtiği grubu kaybettiğini
			// ancak sayfaya bakınca fark ederdi.
			problems = append(problems, Problem{
				Field:   "items",
				Message: fmt.Sprintf("tanınmayan grup: %q", item.Group),
			})

			break
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

func validateGroups(names []string) []Problem {
	if len(names) > MaxGroups {
		return []Problem{{
			Field:   "groups",
			Message: fmt.Sprintf("en fazla %d grup tanımlanabilir", MaxGroups),
		}}
	}

	seen := make(map[string]struct{}, len(names))

	for _, name := range names {
		switch {
		case name == "":
			return []Problem{{Field: "groups", Message: "grup adı boş olamaz"}}
		case utf8.RuneCountInString(name) > MaxGroupNameLength:
			return []Problem{{
				Field:   "groups",
				Message: fmt.Sprintf("grup adı en fazla %d karakter olabilir", MaxGroupNameLength),
			}}
		}

		if _, dup := seen[name]; dup {
			// Aynı sayfada iki "API" grubu ziyaretçi için ayırt edilemez
			// (şemadaki UNIQUE de reddederdi — burada anlaşılır bir mesajla).
			return []Problem{{Field: "groups", Message: "aynı grup adı iki kez gönderilemez"}}
		}

		seen[name] = struct{}{}
	}

	return nil
}
