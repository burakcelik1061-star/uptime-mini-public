package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrNotFound, kanal yoksa VEYA başka bir organizasyona aitse döner.
//
// İki durum TEK hataya indirgeniyor (1.4'ün `orgs.ErrNoAccess` disiplini):
// "var ama senin değil" bilgisi, denenen kimliklerden varlık haritası
// çıkarmaya izin verirdi.
var ErrNotFound = errors.New("bildirim kanalı bulunamadı")

// ErrDuplicate, aynı org'da aynı tür + hedef ikinci kez eklendiğinde döner.
var ErrDuplicate = errors.New("bu hedef zaten ekli")

// ErrUnsupportedKind, kayıtta karşılığı olmayan bir kanal türü için döner.
var ErrUnsupportedKind = errors.New("desteklenmeyen kanal türü")

// ErrTargetBlocked, hedefin SSRF koruması tarafından reddedildiğini bildirir
// (7.4).
//
// AYRI BİR HATA OLMASI ŞART: bu, sunucunun arızası değil KULLANICININ
// yapılandırması. Genel bir gönderim hatası olarak dönseydi çağıran 500
// alırdı ("beklenmeyen bir hata") ve kullanıcı, yazdığı adresin neden hiç
// denenmediğini asla öğrenemezdi.
var ErrTargetBlocked = errors.New("hedef adres izin verilmeyen bir ağa ait")

const (
	// MaxChannels, org başına kanal sayısı üst sınırı. Her kesinti her kanala
	// bir gönderim üretiyor; sınırsız kanal, tek bir kesintinin sınırsız iş
	// doğurması demekti.
	MaxChannels = 20

	maxChannelName   = 80
	maxWebhookTarget = 2048
	maxEmailTarget   = 254 // RFC 5321'in yol sınırı (request.go ile aynı)
)

// Problem / ValidationError, alan bazlı doğrulama sonucu (diğer paketlerdeki
// ikizlerinden bilinçli olarak bağımsız: hiçbiri HTTP'yi tanımıyor).
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

	return "bildirim kanalı doğrulanamadı (" + strings.Join(parts, ", ") + ")"
}

// Store, servisin ihtiyaç duyduğu sorgular.
type Store interface {
	ListNotificationChannels(ctx context.Context, orgID uuid.UUID) ([]store.NotificationChannel, error)
	CreateNotificationChannel(
		ctx context.Context, arg store.CreateNotificationChannelParams,
	) (store.NotificationChannel, error)
	GetNotificationChannel(
		ctx context.Context, arg store.GetNotificationChannelParams,
	) (store.NotificationChannel, error)
	DeleteNotificationChannel(ctx context.Context, arg store.DeleteNotificationChannelParams) (int64, error)
	CountDeliveriesByStatus(ctx context.Context, channelIds []uuid.UUID) ([]store.CountDeliveriesByStatusRow, error)
}

// Service, bildirim kanallarının org-scoped yüzü.
type Service struct {
	queries  Store
	registry *Registry
	logger   *slog.Logger
}

// NewService, servisi kurar.
func NewService(queries Store, registry *Registry, logger *slog.Logger) *Service {
	return &Service{queries: queries, registry: registry, logger: logger}
}

// ChannelInput, yeni bir kanalın girdisi.
type ChannelInput struct {
	Kind   store.NotificationChannelKind
	Name   string
	Target string
}

// Stats, bir kanalın gönderim özeti.
type Stats struct {
	Sent    int64
	Pending int64
	Failed  int64
}

// ChannelWithStats, ekranın gördüğü hâl.
type ChannelWithStats struct {
	Channel store.NotificationChannel
	Stats   Stats
}

// Normalize, girdiyi saklama biçimine getirir.
//
// Doğrulamadan AYRI (2.1'den beri aynı desen): normalleştirme girdiyi
// değiştiriyor, doğrulama yalnızca yargılıyor. İkisi tek fonksiyonda olsaydı,
// "boşluk kırpıldıktan sonra boş kalan ad" gibi durumlar sıraya bağımlı hâle
// gelirdi.
func Normalize(in ChannelInput) ChannelInput {
	in.Name = strings.TrimSpace(in.Name)
	in.Target = strings.TrimSpace(in.Target)

	if in.Kind == store.NotificationChannelKindEmail {
		// E-posta adresleri 1.1'den beri küçük harfe indiriliyor: aynı adresin
		// iki farklı yazımı iki kanal olmasın.
		in.Target = strings.ToLower(in.Target)
	}

	return in
}

// Validate, girdiyi doğrular.
//
// DOĞRULAMA HANDLER'DA DEĞİL SERVİSTE (Faz 2'den beri): kural türe göre
// değişiyor ve HTTP'yi tanımayan tek bir yerde durması, test edilebilir olması
// demek.
func Validate(in ChannelInput) error {
	var problems []Problem

	switch in.Kind {
	case store.NotificationChannelKindEmail, store.NotificationChannelKindWebhook:
	default:
		problems = append(problems, Problem{Field: "kind", Message: "email veya webhook olmalı"})
	}

	switch {
	case in.Name == "":
		problems = append(problems, Problem{Field: "name", Message: "zorunlu"})
	case utf8.RuneCountInString(in.Name) > maxChannelName:
		problems = append(problems, Problem{
			Field:   "name",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", maxChannelName),
		})
	}

	problems = append(problems, validateTarget(in)...)

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

func validateTarget(in ChannelInput) []Problem {
	if in.Target == "" {
		return []Problem{{Field: "target", Message: "zorunlu"}}
	}

	switch in.Kind {
	case store.NotificationChannelKindEmail:
		if len(in.Target) > maxEmailTarget {
			return []Problem{{
				Field:   "target",
				Message: fmt.Sprintf("en fazla %d karakter olabilir", maxEmailTarget),
			}}
		}

		addr, err := mail.ParseAddress(in.Target)
		if err != nil || addr.Address != in.Target || !strings.Contains(strings.SplitN(in.Target, "@", 2)[1], ".") {
			return []Problem{{Field: "target", Message: "geçerli bir e-posta adresi olmalı"}}
		}

	case store.NotificationChannelKindWebhook:
		if len(in.Target) > maxWebhookTarget {
			return []Problem{{
				Field:   "target",
				Message: fmt.Sprintf("en fazla %d karakter olabilir", maxWebhookTarget),
			}}
		}

		// YALNIZCA https (5.3'ün logo kararıyla aynı gerekçe artı bir tane
		// daha): kesinti bildirimi organizasyonun iç durumunu taşıyor ve düz
		// metin bir kanaldan gönderilmemeli. `javascript:`/`data:` gibi şemalar
		// da böylece kapanıyor.
		//
		// Adresin GÜVENLİ olduğu burada doğrulanmıyor — iç ağ/loopback
		// adreslerine karşı koruma (SSRF, 7.4) isteği ATAN tarafta, bağlantı
		// katmanında: bkz. webhook.go → HTTPWebhookChannel ve internal/safedial.
		// Burada denetlemek DNS rebinding'e açık olurdu (ad doğrulamada dış,
		// bağlantıda iç adrese çözülür).
		if !strings.HasPrefix(in.Target, "https://") {
			return []Problem{{Field: "target", Message: "https:// ile başlamalı"}}
		}
	}

	return nil
}

// List, organizasyonun kanallarını gönderim özetleriyle döner.
func (s *Service) List(ctx context.Context, orgID uuid.UUID) ([]ChannelWithStats, error) {
	channels, err := s.queries.ListNotificationChannels(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("bildirim kanalları okunamadı: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.ID)
	}

	stats := make(map[uuid.UUID]Stats, len(channels))

	if len(ids) > 0 {
		rows, err := s.queries.CountDeliveriesByStatus(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("gönderim özetleri okunamadı: %w", err)
		}

		for _, row := range rows {
			entry := stats[row.ChannelID]

			switch row.Status {
			case store.NotificationStatusSent:
				entry.Sent = row.Total
			case store.NotificationStatusPending:
				entry.Pending = row.Total
			case store.NotificationStatusFailed:
				entry.Failed = row.Total
			}

			stats[row.ChannelID] = entry
		}
	}

	result := make([]ChannelWithStats, 0, len(channels))
	for _, channel := range channels {
		result = append(result, ChannelWithStats{Channel: channel, Stats: stats[channel.ID]})
	}

	return result, nil
}

// Create, yeni bir kanal ekler.
func (s *Service) Create(
	ctx context.Context, orgID uuid.UUID, in ChannelInput,
) (store.NotificationChannel, error) {
	in = Normalize(in)
	if err := Validate(in); err != nil {
		return store.NotificationChannel{}, err
	}

	existing, err := s.queries.ListNotificationChannels(ctx, orgID)
	if err != nil {
		return store.NotificationChannel{}, fmt.Errorf("bildirim kanalları okunamadı: %w", err)
	}

	if len(existing) >= MaxChannels {
		return store.NotificationChannel{}, &ValidationError{Problems: []Problem{{
			Field:   "kind",
			Message: fmt.Sprintf("en fazla %d kanal eklenebilir", MaxChannels),
		}}}
	}

	channel, err := s.queries.CreateNotificationChannel(ctx, store.CreateNotificationChannelParams{
		OrgID:  orgID,
		Kind:   in.Kind,
		Name:   in.Name,
		Target: in.Target,
	})
	if err != nil {
		// Çakışmanın hakemi VERİTABANI: "önce SELECT et, yoksa yaz" iki
		// eşzamanlı istekte iki satır üretirdi (1.3'ün slug döngüsüyle aynı
		// gerekçe).
		if postgres.IsUniqueViolation(err, "notification_channels_org_id_kind_target_key") {
			return store.NotificationChannel{}, ErrDuplicate
		}

		return store.NotificationChannel{}, fmt.Errorf("bildirim kanalı oluşturulamadı: %w", err)
	}

	return channel, nil
}

// Delete, kanalı siler.
func (s *Service) Delete(ctx context.Context, orgID, channelID uuid.UUID) error {
	affected, err := s.queries.DeleteNotificationChannel(ctx, store.DeleteNotificationChannelParams{
		ID:    channelID,
		OrgID: orgID,
	})
	if err != nil {
		return fmt.Errorf("bildirim kanalı silinemedi: %w", err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// SendTest, kanala TEST bildirimi gönderir.
//
// DEFTERE YAZILMIYOR ve bu bilinçli: defterin işi "şu KESİNTİ şu kanala
// bildirildi mi" sorusunu cevaplamak. Test bildirimi bir kesintiye ait değil;
// deftere yazsaydık, olmayan bir incident'a bağ kurmak ya da tabloya
// "incident'sız satır" kavramı eklemek gerekirdi.
//
// SENKRON çalışıyor: kullanıcı düğmeye basıp sonucu görmek istiyor. Gerçek
// bildirimler ise asenkron (worker job'ı) — çünkü orada bekleyen bir kullanıcı
// yok ve gönderim kesinti yazmayı bekletmemeli.
func (s *Service) SendTest(ctx context.Context, orgID, channelID uuid.UUID, orgName string) error {
	channel, err := s.queries.GetNotificationChannel(ctx, store.GetNotificationChannelParams{
		ID:    channelID,
		OrgID: orgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}

		return fmt.Errorf("bildirim kanalı okunamadı: %w", err)
	}

	adapter, err := s.registry.For(channel.Kind)
	if err != nil {
		return err
	}

	msg := Message{
		Event:     store.NotificationEventIncidentOpened,
		OrgName:   orgName,
		Title:     "Test bildirimi",
		Impact:    store.IncidentImpactMinor,
		StartedAt: channel.CreatedAt,
	}

	if err := adapter.Send(ctx, channel.Target, msg); err != nil {
		return fmt.Errorf("test bildirimi gönderilemedi: %w", err)
	}

	s.logger.Info("test bildirimi gönderildi",
		slog.String("org_id", orgID.String()),
		slog.String("channel_id", channelID.String()),
		slog.String("kind", string(channel.Kind)))

	return nil
}
