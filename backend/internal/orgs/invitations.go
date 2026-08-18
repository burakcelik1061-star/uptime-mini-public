package orgs

// DAVET AKIŞI (Faz 8.3).
//
// 1.3 üyeliği veritabanı seviyesinde kurmuş, davet akışını açıkça MVP dışına
// almıştı. Sonucu şuydu: 1.4'ün özenle kurduğu çok kiracılılık pratikte TEK
// KULLANICILI kalıyordu — ikinci bir kişi organizasyona ancak elle SQL ile
// girebiliyordu. Bu dosya o boşluğu kapatıyor.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/postgres"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/token"
)

var (
	// ErrInvalidEmail, adres biçim kontrolünden geçmediğinde döner.
	ErrInvalidEmail = errors.New("geçerli bir e-posta adresi olmalı")
	// ErrInvalidRole, tanınmayan bir rol istendiğinde döner.
	ErrInvalidRole = errors.New("rol owner, admin veya member olmalı")
	// ErrRoleTooHigh, davet eden kendi rolünün ÜSTÜNDE bir rol vermeye
	// çalıştığında döner (yetki yükseltme).
	ErrRoleTooHigh = errors.New("kendi rolünüzün üstünde bir rol veremezsiniz")
	// ErrInvitationNotFound, token tanınmıyorsa veya çoktan kullanılmışsa döner.
	ErrInvitationNotFound = errors.New("davet bulunamadı")
	// ErrInvitationExpired, davetin süresi dolmuşsa döner.
	ErrInvitationExpired = errors.New("davetin süresi dolmuş")
	// ErrInvitationEmailMismatch, oturumdaki adres davet edilen adresten
	// farklıysa döner.
	ErrInvitationEmailMismatch = errors.New("bu davet başka bir e-posta adresi için gönderildi")
)

const (
	// invitationTTL, davetin geçerlilik süresi.
	//
	// Yedi gün: bir kişinin e-postasını görüp harekete geçmesi için fazlasıyla
	// yeterli, "sonsuza kadar geçerli organizasyon anahtarı" olacak kadar uzun
	// değil. Süresi dolan davet SİLİNMİYOR — ekranda "süresi doldu" olarak
	// duruyor ve yöneticinin yenisini göndermesi görünür bir hareket oluyor.
	invitationTTL = 7 * 24 * time.Hour

	maxInviteEmailLength = 254

	// invitationsPendingConstraint, kısmi unique indeksin adı (migration 00019).
	invitationsPendingConstraint = "organization_invitations_pending_idx"
)

// Invitation, dışa verilen davet gösterimi.
type Invitation struct {
	ID             uuid.UUID
	Email          string
	Role           store.OrgRole
	ExpiresAt      time.Time
	CreatedAt      time.Time
	InvitedByEmail *string
}

// AcceptedInvitation, kabul sonucunda çağırana dönen bilgi.
type AcceptedInvitation struct {
	OrgID   uuid.UUID
	OrgName string
	Role    store.OrgRole
}

// NormalizeEmail, adresi saklama/karşılaştırma biçimine getirir (1.1'in kuralı).
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ParseRole, istemciden gelen rol metnini enum'a çevirir.
//
// TANINMAYAN DEĞER HATA, sessizce `member`a düşmüyor: 1.4'ün "tanımıyorum ile
// izin ver aynı şey değil" kuralının yazma tarafındaki karşılığı.
func ParseRole(raw string) (store.OrgRole, error) {
	switch store.OrgRole(strings.TrimSpace(raw)) {
	case store.OrgRoleOwner:
		return store.OrgRoleOwner, nil
	case store.OrgRoleAdmin:
		return store.OrgRoleAdmin, nil
	case store.OrgRoleMember:
		return store.OrgRoleMember, nil
	default:
		return "", ErrInvalidRole
	}
}

// validateInviteEmail, biçim kontrolü yapar (1.1'in `validateEmail`iyle aynı
// kurallar; oradaki kopya httpapi'ye ait ve bu katman handler'a bağımlı değil).
func validateInviteEmail(email string) error {
	if email == "" || len(email) > maxInviteEmailLength {
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

// Invite, bir adrese davet gönderir.
//
// ÜÇ DURUM AYNI CEVABI ALIYOR — yeni adres · zaten bekleyen davet · zaten üye
// (6.4'ün enumeration kararı). Gerekçe burada 6.4'tekinden BİR ADIM DAHA
// ileri gidiyor ve asıl sebep bu:
//
//   - "Zaten üye" farkını göstermek, davet edenin zaten görebildiği bir şeyi
//     söylerdi (üye listesi ekranda) — yani tek başına zararsız.
//   - AMA "bu adresin PLATFORMDA hesabı var mı" farkını göstermek, org'un
//     dışına taşan gerçek bir sızıntı olurdu: bir yönetici, kendi
//     organizasyonundan rakip firmanın adreslerini tek tek deneyerek kimin
//     bu ürünü kullandığını çıkarabilirdi.
//
// Bu yüzden akış kullanıcı tablosuna HİÇ BAKMIYOR: davet bir adrese yazılıyor,
// kullanıcının var olup olmadığı sorusu hiç sorulmuyor (bkz. migration 00019 —
// tabloda `user_id` yok). Sorulmayan sorunun cevabı sızamaz.
func (s *Service) Invite(
	ctx context.Context,
	inviter Membership,
	rawEmail string,
	role store.OrgRole,
	publicBaseURL string,
) error {
	email := NormalizeEmail(rawEmail)
	if err := validateInviteEmail(email); err != nil {
		// Biçim hatası SIZINTI DEĞİL: adresin kayıtlı olup olmadığı hakkında
		// bir şey söylemiyor, yalnızca yazım hatasını gösteriyor (6.4'ün
		// abonelik ucundaki aynı ayrım).
		return err
	}

	// YETKİ YÜKSELTME KAPISI. "En az şu rol" sıralaması (1.4) burada TERSİNE
	// okunuyor: davet eden, VERECEĞİ rolün gerektirdiği yetkiye kendisi sahip
	// olmalı. Yani admin admin ve member davet edebiliyor, owner davet
	// EDEMİYOR. Bu kontrol olmasaydı bir admin kendine owner davet edip
	// ikinci bir hesapla organizasyonu devralabilirdi.
	if !inviter.HasAtLeast(role) {
		return ErrRoleTooHigh
	}

	raw, err := token.New()
	if err != nil {
		return err
	}

	hash := token.Hash(raw)
	expiresAt := time.Now().Add(invitationTTL)

	_, err = s.queries.CreateInvitation(ctx, store.CreateInvitationParams{
		OrgID:     inviter.OrgID,
		Email:     email,
		Role:      role,
		TokenHash: &hash,
		InvitedBy: &inviter.UserID,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		// BEKLEYEN DAVET ZATEN VAR: kısmi unique indeksin `23505`i.
		// Çağırana BAŞARI dönüyor — yukarıdaki enumeration kararı.
		//
		// Yeni bir e-posta GÖNDERİLMİYOR: gönderilseydi, "davet et"e üst üste
		// basan biri o adrese istediği kadar posta yollatabilirdi (6.4'ün
		// "zaten onaylı aboneye yeni onay e-postası yok" kararının aynısı).
		if postgres.IsUniqueViolation(err, invitationsPendingConstraint) {
			s.logger.Info("bekleyen davet zaten var, yeni e-posta gönderilmedi",
				slog.String("org_id", inviter.OrgID.String()))

			return nil
		}

		return fmt.Errorf("davet yazılamadı: %w", err)
	}

	s.logger.Info("davet oluşturuldu",
		slog.String("org_id", inviter.OrgID.String()),
		slog.String("role", string(role)),
		slog.String("invited_by", inviter.UserID.String()))

	return s.sendInvitation(ctx, email, inviter.OrgName, role, raw, publicBaseURL)
}

// sendInvitation, davet e-postasını gönderir.
//
// GÖNDERİM 6.3'ÜN AYNI SOYUTLAMASINDAN geçiyor (`notifications.Channel`):
// ikinci bir e-posta katmanı, gerçek sağlayıcıya geçişi iki yerde yapmak
// demekti. 8.2'den beri aynı adapter mesajı posta kutusuna da yazıyor, yani
// bu link terminal açmadan tıklanabiliyor.
func (s *Service) sendInvitation(
	ctx context.Context, email, orgName string, role store.OrgRole, raw, publicBaseURL string,
) error {
	// LİNK ARAYÜZE İŞARET EDİYOR, api'ye değil: tıklayan kişi bir JSON cevabı
	// değil, ne olduğunu anlatan bir sayfa görmeli (6.4'ün kararı).
	link := strings.TrimRight(publicBaseURL, "/") + "/invitations/accept?token=" + raw

	body := fmt.Sprintf(
		"%s organizasyonuna %s rolüyle davet edildiniz.\nKatılmak için: %s\n\n"+
			"Bu daveti siz beklemiyorsanız bir şey yapmanıza gerek yok; bağlantı %d gün sonra geçersiz olacak.",
		orgName, roleLabel(role), link, int(invitationTTL.Hours()/24),
	)

	// İŞLEM E-POSTASI, kesinti bildirimi DEĞİL: konu satırına "Kesinti:"
	// öneki konmasın (bkz. notifications.MessageKind).
	msg := notifications.Message{
		Kind:    notifications.MessageKindTransactional,
		OrgName: orgName,
		Title:   "Organizasyona davet edildiniz",
		Body:    body,
	}

	if err := s.mailer.Send(ctx, email, msg); err != nil {
		return fmt.Errorf("davet e-postası gönderilemedi: %w", err)
	}

	return nil
}

// roleLabel, rolün kullanıcıya gösterilen adı.
func roleLabel(role store.OrgRole) string {
	switch role {
	case store.OrgRoleOwner:
		return "sahip"
	case store.OrgRoleAdmin:
		return "yönetici"
	case store.OrgRoleMember:
		return "üye"
	default:
		return string(role)
	}
}

// ListInvitations, organizasyonun bekleyen davetlerini döner.
func (s *Service) ListInvitations(ctx context.Context, orgID uuid.UUID) ([]Invitation, error) {
	rows, err := s.queries.ListPendingInvitations(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("davetler okunamadı: %w", err)
	}

	invitations := make([]Invitation, 0, len(rows))
	for _, row := range rows {
		invitations = append(invitations, Invitation{
			ID:             row.ID,
			Email:          row.Email,
			Role:           row.Role,
			ExpiresAt:      row.ExpiresAt,
			CreatedAt:      row.CreatedAt,
			InvitedByEmail: row.InvitedByEmail,
		})
	}

	return invitations, nil
}

// RevokeInvitation, bekleyen bir daveti iptal eder.
func (s *Service) RevokeInvitation(ctx context.Context, orgID, invitationID uuid.UUID) error {
	rows, err := s.queries.DeleteInvitation(ctx, store.DeleteInvitationParams{
		ID:    invitationID,
		OrgID: orgID,
	})
	if err != nil {
		return fmt.Errorf("davet iptal edilemedi: %w", err)
	}

	if rows == 0 {
		// "Yok" ile "başkasının" AYNI cevap (1.4'ün ve 2.1'in disiplini).
		return ErrInvitationNotFound
	}

	return nil
}

// AcceptInvitation, daveti kabul eder ve kullanıcıyı organizasyona alır.
//
// TEK TRANSACTION (1.3'ün "sahipsiz organizasyon olmasın" kararının aynı
// gerekçesi): davet "kullanıldı" işaretlenip üyelik yazılamazsa, kullanıcı
// hiçbir organizasyona giremeden linkini de kaybederdi — geri dönüşü olmayan
// bir kayıp. Ters sırada (önce üyelik) ise davet kullanılabilir kalır ve token
// tek kullanımlığını yitirirdi.
func (s *Service) AcceptInvitation(
	ctx context.Context, userID uuid.UUID, userEmail, rawToken string,
) (AcceptedInvitation, error) {
	hash := token.Hash(rawToken)

	invitation, err := s.queries.GetInvitationByTokenHash(ctx, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Token tanınmıyor VEYA çoktan kullanılmış (kullanımda hash NULL'a
			// iniyor): ikisi de aynı cevap.
			return AcceptedInvitation{}, ErrInvitationNotFound
		}

		return AcceptedInvitation{}, fmt.Errorf("davet okunamadı: %w", err)
	}

	if invitation.AcceptedAt != nil {
		return AcceptedInvitation{}, ErrInvitationNotFound
	}

	if time.Now().After(invitation.ExpiresAt) {
		// Süre farkı AYRI bir cevap: kullanıcının yapacağı iş farklı
		// ("yenisini iste" vs. "linkin yanlış"). Token zaten tahmin edilemez
		// olduğu için bu ayrım kimseye bilgi vermiyor.
		return AcceptedInvitation{}, ErrInvitationExpired
	}

	// ADRES EŞLEŞMESİ ŞART. Token tek başına yeterli OLSAYDI, sızan bir link
	// (iletilen e-posta, paylaşılan ekran, erişim logu) organizasyona giriş
	// anahtarı olurdu. Eşleşme şartıyla token'ın verdiği yetenek daralıyor:
	// "BU adresin sahibi organizasyona katılsın" — 6.4'ün onay token'ındaki
	// "adres sahipliğini kanıtla" mantığının aynısı.
	//
	// Hata mesajı davet edilen adresi YAZMIYOR: linki ele geçiren birine
	// hedefi söylemenin bir faydası yok, meşru kullanıcı ise daveti hangi
	// posta kutusunda gördüğünü zaten biliyor.
	if NormalizeEmail(userEmail) != invitation.Email {
		return AcceptedInvitation{}, ErrInvitationEmailMismatch
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return AcceptedInvitation{}, fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	// SIRA ÖNEMLİ DEĞİL ama YARIŞ KAPISI burada: `accepted_at IS NULL`
	// koşulunu taşıyan UPDATE 0 satır etkilerse, aynı daveti iki sekmede açan
	// kullanıcının ikinci isteği tümden geri alınıyor.
	consumed, err := q.AcceptInvitation(ctx, invitation.ID)
	if err != nil {
		return AcceptedInvitation{}, fmt.Errorf("davet işaretlenemedi: %w", err)
	}
	if consumed == 0 {
		return AcceptedInvitation{}, ErrInvitationNotFound
	}

	if _, err := q.AddOrganizationMemberIfAbsent(ctx, store.AddOrganizationMemberIfAbsentParams{
		OrgID:  invitation.OrgID,
		UserID: userID,
		Role:   invitation.Role,
	}); err != nil {
		return AcceptedInvitation{}, fmt.Errorf("üyelik yazılamadı: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return AcceptedInvitation{}, fmt.Errorf("davet kaydedilemedi: %w", err)
	}

	s.logger.Info("davet kabul edildi",
		slog.String("org_id", invitation.OrgID.String()),
		slog.String("user_id", userID.String()),
		slog.String("role", string(invitation.Role)))

	return AcceptedInvitation{
		OrgID:   invitation.OrgID,
		OrgName: invitation.OrgName,
		Role:    invitation.Role,
	}, nil
}
