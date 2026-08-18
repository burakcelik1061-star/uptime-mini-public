// Package maintenance, PLANLI bakım pencerelerini yönetir (Faz 4.3).
//
// Pencerenin iki etkisi var ve ikisi de bu paketin dışında yaşıyor:
//
//	internal/incidents → pencere içindeyken otomatik incident AÇILMIYOR
//	db/queries/rollups → pencereye düşen kontroller uptime'ın paydasından
//	                     düşülüyor ama `maintenance_checks` olarak sayılıyor
//
// Yani burası yalnızca "hangi servis, ne zaman, neden bakımda" kaydını tutuyor;
// kararı okuyan taraflar kendi sorgularıyla bakıyor. Alternatif — pencereyi
// açarken monitor'lere bir bayrak yazmak — kapanışta ikinci bir yazma
// gerektirir ve worker/api'den biri o anda çalışmıyorsa bayrak asılı kalırdı.
package maintenance

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Sınırlar (şemadaki CHECK kısıtlarıyla aynı; buradaki kopya kullanıcıya alan
// bazlı hata verebilmek için — 2.1'in "uygulama açıklar, veritabanı son hakem"
// disiplini).
const (
	MaxTitleLength       = 200
	MaxDescriptionLength = 2000

	// MaxMonitorsPerWindow, tek bir pencerenin kapsayabileceği servis sayısı.
	// Sunucunun kendini koruması; org'un monitor sayısı bunu aşarsa pencereyi
	// bölmek gerekir.
	MaxMonitorsPerWindow = 100

	// MinDuration, pencerenin en kısa süresi. Saniyelik bir "bakım" bir
	// yapılandırma hatasıdır, bakım değil.
	MinDuration = time.Minute

	// MaxDuration, en uzun süre: 30 gün. Sınırsız bırakmak, unutulan bir
	// pencerenin aylarca incident'ları bastırması demekti.
	MaxDuration = 30 * 24 * time.Hour

	// DefaultListLimit / MaxListLimit, liste ucunun sınırları.
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ErrNotFound, pencere yoksa VEYA başka bir organizasyona aitse döner.
var ErrNotFound = errors.New("bakım penceresi bulunamadı")

// ErrStarted, başlamış bir pencere silinmeye çalışıldığında döner.
//
// Neden silinemiyor: başlamış bir pencereyi kaldırmak GEÇMİŞİ YENİDEN YAZMAK
// demek. O pencereye düşen kontroller `maintenance_checks` olarak çoktan
// özetlenmiş olabilir (3.4 kapanmış kovaları yeniden hesaplamıyor), yani aynı
// gün için iki farklı uptime cevabı doğardı.
var ErrStarted = errors.New("bakım penceresi başlamış")

// Problem / ValidationError — `monitors` ve `incidents` paketlerindeki
// ikizlerinden bağımsız (paketler birbirini tanımıyor, HTTP'yi hiçbiri
// tanımıyor).
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

	return "bakım penceresi doğrulanamadı (" + strings.Join(parts, ", ") + ")"
}

// Store, bu servisin veritabanından ihtiyaç duyduğu sorgular.
type Store interface {
	CreateMaintenanceWindow(
		ctx context.Context, arg store.CreateMaintenanceWindowParams,
	) (store.MaintenanceWindow, error)
	AddMaintenanceWindowMonitors(ctx context.Context, arg store.AddMaintenanceWindowMonitorsParams) (int64, error)
	ListMaintenanceWindowsForOrg(
		ctx context.Context, arg store.ListMaintenanceWindowsForOrgParams,
	) ([]store.MaintenanceWindow, error)
	GetMaintenanceWindowForOrg(
		ctx context.Context, arg store.GetMaintenanceWindowForOrgParams,
	) (store.MaintenanceWindow, error)
	ListMaintenanceWindowMonitors(
		ctx context.Context, arg store.ListMaintenanceWindowMonitorsParams,
	) ([]store.ListMaintenanceWindowMonitorsRow, error)
	DeleteUpcomingMaintenanceWindow(
		ctx context.Context, arg store.DeleteUpcomingMaintenanceWindowParams,
	) (int64, error)
}

// Service, bakım pencerelerinin CRUD'u.
//
// Havuza doğrudan ihtiyaç var: pencere ve kapsadığı servisler TEK
// TRANSACTION'da yazılıyor (1.3'ün "sahipsiz organizasyon olmasın"
// kararının aynısı — hiçbir servisi kapsamayan bir bakım penceresi hiçbir
// şey yapmayan bir kayıttır).
type Service struct {
	pool    *pgxpool.Pool
	queries Store
	logger  *slog.Logger

	// now, testlerin saati sabitleyebilmesi için (rollups ile aynı desen).
	now func() time.Time
}

// NewService, servisi kurar.
func NewService(pool *pgxpool.Pool, queries Store, logger *slog.Logger) *Service {
	return &Service{pool: pool, queries: queries, logger: logger, now: time.Now}
}

// AffectedMonitor, pencerenin kapsadığı bir servis.
type AffectedMonitor struct {
	ID     uuid.UUID
	Name   string
	Status store.MonitorStatus
}

// Window, pencere + kapsadığı servisler.
type Window struct {
	Window   store.MaintenanceWindow
	Monitors []AffectedMonitor
}

// Input, yeni bir pencerenin alanları.
type Input struct {
	Title       string
	Description *string
	StartsAt    time.Time
	EndsAt      time.Time
	MonitorIDs  []uuid.UUID
}

// Create, ileri tarihli bir bakım penceresi tanımlar.
//
// Pencere ve kapsadığı servisler TEK TRANSACTION'da yazılıyor; ayrıca bağ
// sorgusu yalnızca AYNI ORGANİZASYONUN monitor'lerini bağlıyor ve yazılan
// satır sayısı istenenle karşılaştırılıyor — listede yabancı ya da olmayan bir
// kimlik varsa işlem tümden geri alınıyor. "Sessizce daha az servisi kapsayan
// bakım penceresi" en kötü sonuç olurdu: kullanıcı kapsadığını sanır, incident
// yine de açılır.
func (s *Service) Create(ctx context.Context, orgID uuid.UUID, in Input) (Window, error) {
	in = Normalize(in)
	if err := s.Validate(in); err != nil {
		return Window{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Window{}, fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := store.New(tx)

	window, err := queries.CreateMaintenanceWindow(ctx, store.CreateMaintenanceWindowParams{
		OrgID:       orgID,
		Title:       in.Title,
		Description: in.Description,
		StartsAt:    in.StartsAt,
		EndsAt:      in.EndsAt,
	})
	if err != nil {
		return Window{}, fmt.Errorf("bakım penceresi oluşturulamadı: %w", err)
	}

	linked, err := queries.AddMaintenanceWindowMonitors(ctx, store.AddMaintenanceWindowMonitorsParams{
		WindowID:   window.ID,
		MonitorIds: in.MonitorIDs,
		OrgID:      orgID,
	})
	if err != nil {
		return Window{}, fmt.Errorf("bakım penceresine servisler bağlanamadı: %w", err)
	}

	if int(linked) != len(in.MonitorIDs) {
		// Transaction geri alınıyor (defer'daki Rollback): yarım kapsamlı bir
		// pencere bırakmaktansa isteği reddetmek.
		return Window{}, &ValidationError{Problems: []Problem{{
			Field:   "monitorIds",
			Message: "bu organizasyona ait olmayan veya bulunmayan servis var",
		}}}
	}

	rows, err := queries.ListMaintenanceWindowMonitors(ctx, store.ListMaintenanceWindowMonitorsParams{
		OrgID:     orgID,
		WindowIds: []uuid.UUID{window.ID},
	})
	if err != nil {
		return Window{}, fmt.Errorf("bakım penceresinin servisleri okunamadı: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Window{}, fmt.Errorf("bakım penceresi kaydedilemedi: %w", err)
	}

	s.logger.Info("bakım penceresi oluşturuldu",
		slog.String("window_id", window.ID.String()),
		slog.String("org_id", orgID.String()),
		slog.Time("starts_at", window.StartsAt),
		slog.Time("ends_at", window.EndsAt),
		slog.Int("monitors", len(rows)))

	return Window{Window: window, Monitors: affectedFrom(rows)}, nil
}

// List, organizasyonun pencerelerini döndürür (yeniden eskiye).
func (s *Service) List(ctx context.Context, orgID uuid.UUID, limit int32) ([]Window, error) {
	windows, err := s.queries.ListMaintenanceWindowsForOrg(ctx, store.ListMaintenanceWindowsForOrgParams{
		OrgID:    orgID,
		RowLimit: clampLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("bakım pencereleri okunamadı: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(windows))
	for _, window := range windows {
		ids = append(ids, window.ID)
	}

	grouped, err := s.monitorsByWindow(ctx, orgID, ids)
	if err != nil {
		return nil, err
	}

	items := make([]Window, 0, len(windows))
	for _, window := range windows {
		items = append(items, Window{Window: window, Monitors: grouped[window.ID]})
	}

	return items, nil
}

// Delete, HENÜZ BAŞLAMAMIŞ bir pencereyi siler.
//
// Başlamış pencere `ErrStarted` alıyor (bkz. o hatanın gerekçesi). 404 ile 409
// farkını üretebilmek için önce okuma yapılıyor; silme sorgusu koşulu ayrıca
// taşıdığı için okuma ile silme arasında pencere başlarsa da geçmiş
// değişmiyor.
func (s *Service) Delete(ctx context.Context, orgID, windowID uuid.UUID) error {
	window, err := s.queries.GetMaintenanceWindowForOrg(ctx, store.GetMaintenanceWindowForOrgParams{
		WindowID: windowID,
		OrgID:    orgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}

		return fmt.Errorf("bakım penceresi okunamadı: %w", err)
	}

	if !window.StartsAt.After(s.now().UTC()) {
		return ErrStarted
	}

	affected, err := s.queries.DeleteUpcomingMaintenanceWindow(ctx, store.DeleteUpcomingMaintenanceWindowParams{
		WindowID: windowID,
		OrgID:    orgID,
	})
	if err != nil {
		return fmt.Errorf("bakım penceresi silinemedi: %w", err)
	}

	if affected == 0 {
		// Okuma ile silme arasında pencere başlamış olabilir.
		return ErrStarted
	}

	s.logger.Info("bakım penceresi silindi",
		slog.String("window_id", windowID.String()),
		slog.String("org_id", orgID.String()))

	return nil
}

// monitorsByWindow, pencere kimliklerine göre servisleri gruplar.
func (s *Service) monitorsByWindow(
	ctx context.Context, orgID uuid.UUID, windowIDs []uuid.UUID,
) (map[uuid.UUID][]AffectedMonitor, error) {
	grouped := make(map[uuid.UUID][]AffectedMonitor, len(windowIDs))
	if len(windowIDs) == 0 {
		return grouped, nil
	}

	rows, err := s.queries.ListMaintenanceWindowMonitors(ctx, store.ListMaintenanceWindowMonitorsParams{
		OrgID:     orgID,
		WindowIds: windowIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("bakım penceresinin servisleri okunamadı: %w", err)
	}

	for _, row := range rows {
		grouped[row.WindowID] = append(grouped[row.WindowID], AffectedMonitor{
			ID:     row.MonitorID,
			Name:   row.Name,
			Status: row.CurrentStatus,
		})
	}

	return grouped, nil
}

func affectedFrom(rows []store.ListMaintenanceWindowMonitorsRow) []AffectedMonitor {
	out := make([]AffectedMonitor, 0, len(rows))
	for _, row := range rows {
		out = append(out, AffectedMonitor{ID: row.MonitorID, Name: row.Name, Status: row.CurrentStatus})
	}

	return out
}

// Normalize, girdiyi saklama biçimine getirir.
func Normalize(in Input) Input {
	in.Title = strings.TrimSpace(in.Title)

	if in.Description != nil {
		trimmed := strings.TrimSpace(*in.Description)
		if trimmed == "" {
			// Boş açıklama ile "açıklama yok" aynı şey: veritabanında iki farklı
			// gösterimi olmasın.
			in.Description = nil
		} else {
			in.Description = &trimmed
		}
	}

	in.StartsAt = in.StartsAt.UTC()
	in.EndsAt = in.EndsAt.UTC()

	return in
}

// Validate, alan ve alanlar-arası kuralları denetler.
//
// GEÇMİŞE DÖNÜK PENCERE REDDEDİLİYOR (`ends_at` gelecekte olmalı): bitmiş bir
// aralığı sonradan "bakım" ilan etmek, uptime geçmişini yeniden yazmak demek.
// O aralığın özetleri 3.4 tarafından çoktan hesaplanmış olabilir ve kapanmış
// kovalar yeniden hesaplanmıyor — aynı gün için biri özetten biri ham veriden
// gelen iki farklı uptime cevabı doğardı. Başlangıcın geçmişte olmasına ise
// izin var: "şu an devam eden bir bakımı şimdi ilan ediyorum" meşru bir
// senaryo ve o andan sonraki kontroller doğru sayılıyor.
func (s *Service) Validate(in Input) error {
	var problems []Problem

	switch {
	case in.Title == "":
		problems = append(problems, Problem{Field: "title", Message: "zorunlu"})
	case utf8.RuneCountInString(in.Title) > MaxTitleLength:
		problems = append(problems, Problem{
			Field:   "title",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxTitleLength),
		})
	}

	if in.Description != nil && utf8.RuneCountInString(*in.Description) > MaxDescriptionLength {
		problems = append(problems, Problem{
			Field:   "description",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxDescriptionLength),
		})
	}

	problems = append(problems, validateRange(in, s.now().UTC())...)
	problems = append(problems, validateMonitors(in)...)

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

func validateRange(in Input, now time.Time) []Problem {
	var problems []Problem

	switch {
	case in.StartsAt.IsZero():
		problems = append(problems, Problem{Field: "startsAt", Message: "zorunlu"})
	case in.EndsAt.IsZero():
		problems = append(problems, Problem{Field: "endsAt", Message: "zorunlu"})
	case !in.EndsAt.After(in.StartsAt):
		problems = append(problems, Problem{Field: "endsAt", Message: "başlangıçtan sonra olmalı"})
	case !in.EndsAt.After(now):
		problems = append(problems, Problem{Field: "endsAt", Message: "gelecekte olmalı"})
	case in.EndsAt.Sub(in.StartsAt) < MinDuration:
		problems = append(problems, Problem{Field: "endsAt", Message: "en az 1 dakika sürmeli"})
	case in.EndsAt.Sub(in.StartsAt) > MaxDuration:
		problems = append(problems, Problem{Field: "endsAt", Message: "en fazla 30 gün sürebilir"})
	}

	return problems
}

func validateMonitors(in Input) []Problem {
	switch {
	case len(in.MonitorIDs) == 0:
		// Hiçbir servisi kapsamayan pencere hiçbir şey yapmaz; kullanıcı
		// kapsadığını sanır ve bakım sırasında incident açılır.
		return []Problem{{Field: "monitorIds", Message: "en az bir servis seçilmeli"}}
	case len(in.MonitorIDs) > MaxMonitorsPerWindow:
		return []Problem{{
			Field:   "monitorIds",
			Message: fmt.Sprintf("en fazla %d servis seçilebilir", MaxMonitorsPerWindow),
		}}
	}

	seen := make(map[uuid.UUID]struct{}, len(in.MonitorIDs))
	for _, id := range in.MonitorIDs {
		if _, dup := seen[id]; dup {
			// Yinelenen kimlik bağ sorgusunda tek satır yazar ve "yazılan sayı
			// istenenden az" kontrolüne takılırdı: hatayı burada, anlaşılır
			// biçimde veriyoruz.
			return []Problem{{Field: "monitorIds", Message: "aynı servis birden fazla kez gönderilemez"}}
		}
		seen[id] = struct{}{}
	}

	return nil
}

func clampLimit(limit int32) int32 {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	default:
		return limit
	}
}
