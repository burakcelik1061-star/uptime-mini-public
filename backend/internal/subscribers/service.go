package subscribers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrInvalidEmail, adres biçim kontrolünden geçmediğinde döner.
var ErrInvalidEmail = errors.New("geçerli bir e-posta adresi olmalı")

// ErrNotFound, sayfa yoksa/yayımlanmamışsa veya token tanınmıyorsa döner.
var ErrNotFound = errors.New("bulunamadı")

// maxEmailLength, RFC 5321'in yol sınırı (httpapi'deki ikiziyle aynı).
const maxEmailLength = 254

// Store, servisin ihtiyaç duyduğu sorgular.
type Store interface {
	GetPublishedStatusPage(ctx context.Context, slug string) (store.GetPublishedStatusPageRow, error)
	UpsertSubscriber(ctx context.Context, arg store.UpsertSubscriberParams) (store.StatusPageSubscriber, error)
	ConfirmSubscriber(ctx context.Context, confirmTokenHash *string) (store.StatusPageSubscriber, error)
	GetSubscriberByUnsubscribeToken(
		ctx context.Context, unsubscribeToken string,
	) (store.GetSubscriberByUnsubscribeTokenRow, error)
	DeleteSubscriber(ctx context.Context, id uuid.UUID) (int64, error)
	CountConfirmedSubscribers(ctx context.Context, statusPageID uuid.UUID) (int64, error)
}

// Mailer, onay ve iptal linklerini gönderen bağımlılık.
//
// 6.3'ün `notifications.Channel` arayüzünün ta kendisi: ikinci bir e-posta
// soyutlaması yazmak, gerçek sağlayıcıya geçişi İKİ yerde yapmak demekti.
type Mailer interface {
	Send(ctx context.Context, target string, msg notifications.Message) error
}

// Service, abonelik işlemleri.
type Service struct {
	queries Store
	mailer  Mailer
	// publicBaseURL, e-postadaki linklerin kökü (arayüzün adresi).
	publicBaseURL string
	logger        *slog.Logger
}

// NewService, servisi kurar.
func NewService(queries Store, mailer Mailer, publicBaseURL string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		queries:       queries,
		mailer:        mailer,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
		logger:        logger,
	}
}

// NormalizeEmail, adresi saklama/karşılaştırma biçimine getirir (1.1'in kuralı).
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateEmail, biçim kontrolü yapar.
func ValidateEmail(email string) error {
	if email == "" || len(email) > maxEmailLength {
		return ErrInvalidEmail
	}

	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return ErrInvalidEmail
	}

	_, domain, _ := strings.Cut(email, "@")
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return ErrInvalidEmail
	}

	return nil
}

// Subscribe, ziyaretçiyi sayfaya abone eder ve ONAY LİNKİ gönderir.
//
// ENUMERATION'A KAPALI: bu fonksiyon "zaten kayıtlı", "onaylı", "yeni" için
// AYRI SONUÇ ÜRETMİYOR — hepsi aynı yoldan geçip aynı şeyi dönüyor. Sebep
// somut: farklı bir cevap, denenen adreslerden hangilerinin o sayfaya abone
// olduğunu dışarıya söylerdi (1.4'ün "yok ile yetkin yok aynı cevabı alır"
// disiplininin ziyaretçi tarafındaki karşılığı).
//
// Bilinmeyen/yayımlanmamış SAYFA da aynı sebeple ayırt edilmiyor: çağıran
// taraf ErrNotFound'u da başarı gibi cevaplıyor (bkz. httpapi).
func (s *Service) Subscribe(ctx context.Context, slug, rawEmail string) error {
	email := NormalizeEmail(rawEmail)
	if err := ValidateEmail(email); err != nil {
		// Biçim hatası SIZINTI DEĞİL: adresin kayıtlı olup olmadığı hakkında
		// bir şey söylemiyor, yalnızca yazım hatasını gösteriyor.
		return err
	}

	page, err := s.queries.GetPublishedStatusPage(ctx, statusPageSlug(slug))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}

		return fmt.Errorf("status page okunamadı: %w", err)
	}

	confirmToken, err := newToken()
	if err != nil {
		return err
	}

	unsubscribeToken, err := newToken()
	if err != nil {
		return err
	}

	confirmHash := hashToken(confirmToken)

	subscriber, err := s.queries.UpsertSubscriber(ctx, store.UpsertSubscriberParams{
		StatusPageID:     page.ID,
		Email:            email,
		ConfirmTokenHash: &confirmHash,
		UnsubscribeToken: unsubscribeToken,
	})
	if err != nil {
		return fmt.Errorf("abonelik yazılamadı: %w", err)
	}

	// ZATEN ONAYLI ABONEYE YENİ ONAY LİNKİ GÖNDERİLMİYOR: gönderilseydi,
	// bir yabancı başkasının adresini yazarak ona istediği kadar e-posta
	// yollatabilirdi. Cevap yine aynı — çağıran bu ayrımı GÖREMİYOR.
	if subscriber.Status == store.SubscriberStatusConfirmed {
		s.logger.Info("zaten onaylı abone için onay linki gönderilmedi",
			slog.String("status_page_id", page.ID.String()))

		return nil
	}

	// İPTAL TOKEN'I DA ONAY E-POSTASINDA: onay linkine tıklamayan biri, o
	// e-postayı hiç istememiş olabilir ve elinde vazgeçecek bir link olmalı.
	return s.sendConfirmation(ctx, email, page.Slug, page.OrgName, confirmToken, unsubscribeToken)
}

// Confirm, onay token'ıyla aboneliği onaylar.
func (s *Service) Confirm(ctx context.Context, token string) error {
	hash := hashToken(token)

	if _, err := s.queries.ConfirmSubscriber(ctx, &hash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Token tanınmıyor VEYA çoktan kullanılmış: ikisi de aynı cevap.
			// Ayrım üretmek, geçerli token uzayı hakkında bilgi vermek olurdu.
			return ErrNotFound
		}

		return fmt.Errorf("abonelik onaylanamadı: %w", err)
	}

	return nil
}

// Unsubscribe, iptal token'ıyla aboneliği siler.
//
// Token TEK KULLANIMLIK DEĞİL ama ihtiyaç da yok: satır silindiği için ikinci
// çağrı zaten eşleşmiyor.
func (s *Service) Unsubscribe(ctx context.Context, token string) (string, error) {
	row, err := s.queries.GetSubscriberByUnsubscribeToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}

		return "", fmt.Errorf("abonelik okunamadı: %w", err)
	}

	if _, err := s.queries.DeleteSubscriber(ctx, row.ID); err != nil {
		return "", fmt.Errorf("abonelik silinemedi: %w", err)
	}

	s.logger.Info("abonelik iptal edildi", slog.String("status_page_id", row.StatusPageID.String()))

	return row.PageSlug, nil
}

// CountConfirmed, sayfanın onaylı abone sayısı (admin ekranı için).
func (s *Service) CountConfirmed(ctx context.Context, statusPageID uuid.UUID) (int64, error) {
	total, err := s.queries.CountConfirmedSubscribers(ctx, statusPageID)
	if err != nil {
		return 0, fmt.Errorf("abone sayısı okunamadı: %w", err)
	}

	return total, nil
}

// sendConfirmation, çift onay e-postasını gönderir.
func (s *Service) sendConfirmation(ctx context.Context, email, slug, orgName, confirm, unsubscribe string) error {
	// LİNKLER ARAYÜZE İŞARET EDİYOR, api'ye değil: tıklayan kişi bir JSON
	// cevabı değil, ne olduğunu anlatan bir sayfa görmeli.
	body := fmt.Sprintf(
		"%s status sayfasına abone olmak için onaylayın: %s\nVazgeçmek için: %s",
		orgName,
		s.publicBaseURL+"/status/"+slug+"/confirm?token="+confirm,
		s.publicBaseURL+"/status/"+slug+"/unsubscribe?token="+unsubscribe,
	)

	// Mesaj 6.3'ün `Message` yapısıyla taşınıyor: e-posta gönderiminin tek bir
	// soyutlaması var ve gerçek sağlayıcıya geçiş yine tek bir yerde.
	//
	// İŞLEM E-POSTASI (8.3'ün düzeltmesi): daha önce `Event: incident_opened`
	// geçiliyordu ve konu satırı "[Acme] KESİNTİ: Aboneliğinizi onaylayın"
	// oluyordu — onay e-postası kesinti gibi görünüyordu. Hata 8.2'nin posta
	// kutusu ekranı gelene kadar görünmedi (gönderim yalnızca loga yazılıyordu).
	msg := notifications.Message{
		Kind:    notifications.MessageKindTransactional,
		OrgName: orgName,
		Title:   "Aboneliğinizi onaylayın",
		Body:    body,
	}

	if err := s.mailer.Send(ctx, email, msg); err != nil {
		return fmt.Errorf("onay e-postası gönderilemedi: %w", err)
	}

	return nil
}

// statusPageSlug, slug'ı public okuma ucuyla AYNI biçime getirir.
func statusPageSlug(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
