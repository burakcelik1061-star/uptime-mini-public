// Package sessions, refresh token oturumlarını yönetir: açma (giriş),
// rotasyon (yenileme) ve iptal (logout / yeniden kullanım tespiti).
//
// Bu paket HTTP bilmiyor — cookie, status kodu ve hata gövdesi
// internal/httpapi'nin işi. Buradaki tek konu, oturumun veritabanındaki
// durumunun DOĞRU ve YARIŞSIZ değişmesi.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

var (
	// ErrInvalidRefreshToken: token tanınmıyor, süresi dolmuş veya oturum
	// iptal edilmiş. HTTP tarafı üçünü de aynı cevapla karşılamalı.
	ErrInvalidRefreshToken = errors.New("refresh token geçersiz")

	// ErrRefreshTokenReused: DAHA ÖNCE kullanılmış bir token tekrar sunuldu.
	// Oturumun tamamı iptal edildi.
	ErrRefreshTokenReused = errors.New("refresh token yeniden kullanıldı")
)

// Beginner, transaction başlatabilen bağlantı havuzu (*pgxpool.Pool).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Issued, yeni açılan oturum.
type Issued struct {
	SessionID uuid.UUID
	Token     auth.RefreshToken
}

// Rotated, yenilenen oturum.
type Rotated struct {
	SessionID uuid.UUID
	UserID    uuid.UUID
	Token     auth.RefreshToken
}

// Service, oturum akışının veri tarafı.
type Service struct {
	db      Beginner
	queries *store.Queries
	tokens  *auth.RefreshTokenFactory
	logger  *slog.Logger
	now     func() time.Time
}

// NewService, servisi kurar.
func NewService(db Beginner, queries *store.Queries, tokens *auth.RefreshTokenFactory, logger *slog.Logger) *Service {
	return &Service{db: db, queries: queries, tokens: tokens, logger: logger, now: time.Now}
}

// Start, giriş/kayıt sonrası YENİ bir oturum açar.
func (s *Service) Start(ctx context.Context, userID uuid.UUID) (Issued, error) {
	token, err := s.tokens.New()
	if err != nil {
		return Issued{}, err
	}

	sessionID := uuid.New()

	if _, err := s.queries.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		SessionID: sessionID,
		UserID:    userID,
		TokenHash: token.Hash,
		ExpiresAt: token.ExpiresAt,
	}); err != nil {
		return Issued{}, fmt.Errorf("refresh token kaydedilemedi: %w", err)
	}

	return Issued{SessionID: sessionID, Token: token}, nil
}

// Rotate, sunulan refresh token'ı doğrular, KULLANILMIŞ olarak işaretler ve
// aynı oturum ailesinde yenisini üretir.
//
// Tamamı tek bir transaction içinde ve satır kilidiyle (FOR UPDATE) koşuyor:
// aynı token'la eş zamanlı gelen iki istek sıraya giriyor, ikincisi
// `used_at`'i dolmuş buluyor ve yeniden kullanım olarak işleniyor. Kilit
// olmasaydı iki istek de geçerli sayılır, aynı aileden iki paralel zincir
// doğardı.
func (s *Service) Rotate(ctx context.Context, rawToken string) (Rotated, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Rotated{}, fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	// Başarılı yollarda aşağıda Commit ediliyor; bu Rollback yalnızca hata
	// yollarını topluyor (commit'ten sonrası etkisiz).
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	row, err := q.LockRefreshTokenByHash(ctx, s.tokens.Hash(rawToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Rotated{}, ErrInvalidRefreshToken
		}

		return Rotated{}, fmt.Errorf("refresh token okunamadı: %w", err)
	}

	// Oturum zaten iptal edilmiş (logout veya önceki bir reuse tespiti).
	// Yeniden iptal etmeye gerek yok.
	if row.RevokedAt != nil {
		return Rotated{}, ErrInvalidRefreshToken
	}

	// YENİDEN KULLANIM. Bu token'ın geçerli devamı zaten üretilmişti; şimdi
	// aynısı tekrar sunuluyor. İki olasılık var: (a) token çalındı ve
	// saldırgan kullanıyor, (b) meşru istemcinin elinde eski kopya kaldı.
	// İkisini ayırt etmek mümkün değil, bu yüzden güvenli taraf seçiliyor:
	// ailenin tamamı iptal edilir, kullanıcı yeniden giriş yapar.
	if row.UsedAt != nil {
		revoked, err := q.RevokeSession(ctx, row.SessionID)
		if err != nil {
			return Rotated{}, fmt.Errorf("oturum iptal edilemedi: %w", err)
		}

		// İptal KALICI olmalı — bu yolda da commit ediliyor.
		if err := tx.Commit(ctx); err != nil {
			return Rotated{}, fmt.Errorf("iptal kaydedilemedi: %w", err)
		}

		s.logger.Warn("refresh token yeniden kullanıldı, oturum iptal edildi",
			slog.String("session_id", row.SessionID.String()),
			slog.String("user_id", row.UserID.String()),
			slog.Int64("iptal_edilen_token", revoked))

		return Rotated{}, ErrRefreshTokenReused
	}

	if !row.ExpiresAt.After(s.now()) {
		return Rotated{}, ErrInvalidRefreshToken
	}

	next, err := s.tokens.New()
	if err != nil {
		return Rotated{}, err
	}

	if err := q.MarkRefreshTokenUsed(ctx, row.ID); err != nil {
		return Rotated{}, fmt.Errorf("refresh token işaretlenemedi: %w", err)
	}

	if _, err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		// AYNI oturum: zincirin tamamı tek bir session_id altında kalıyor ki
		// iptal tek hamlede yapılabilsin.
		SessionID: row.SessionID,
		UserID:    row.UserID,
		TokenHash: next.Hash,
		ExpiresAt: next.ExpiresAt,
	}); err != nil {
		return Rotated{}, fmt.Errorf("yeni refresh token kaydedilemedi: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Rotated{}, fmt.Errorf("yenileme kaydedilemedi: %w", err)
	}

	return Rotated{SessionID: row.SessionID, UserID: row.UserID, Token: next}, nil
}

// Revoke, logout: sunulan token'ın oturum ailesini kapatır.
//
// Bilinmeyen token sessizce başarılı sayılıyor. Logout ETKİSİ aynı ("artık
// oturum yok") ve hata döndürmek, elindeki değerin sistemde var olup
// olmadığını öğrenmek isteyen birine bilgi verirdi.
func (s *Service) Revoke(ctx context.Context, rawToken string) error {
	row, err := s.queries.LockRefreshTokenByHash(ctx, s.tokens.Hash(rawToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		return fmt.Errorf("refresh token okunamadı: %w", err)
	}

	if _, err := s.queries.RevokeSession(ctx, row.SessionID); err != nil {
		return fmt.Errorf("oturum iptal edilemedi: %w", err)
	}

	return nil
}
