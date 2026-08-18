// Package passwordreset, parolasını unutan kullanıcının hesabını geri
// almasını sağlar (Faz 8.4).
//
// 1.1 bu akışı "dış e-posta servisi yok" gerekçesiyle ertelemişti; sonucu
// somuttu — parolasını unutan kullanıcı KALICI OLARAK kilitliydi.
//
// # Bu paketin belirleyici kararı: CEVAP SÜRESİ SIZDIRMAMALI
//
// İstek ucu kimliksiz bir yabancının çağırabildiği bir uç. "Bu adres kayıtlı
// değil" bilgisini GÖVDEDE vermemek yetmiyor: kayıtlı adres için token
// üretilip e-posta gönderiliyor, kayıtsız adres için hiçbir şey yapılmıyor —
// yani CEVAP SÜRESİ ikisini ayırt ediyor ve uç sessizce bir "bu adres bu
// üründe kayıtlı mı" sorgulayıcısına dönüşüyor.
//
// Çözüm zinciri şöyle kuruldu:
//
//  1. YAPAY GECİKME YOK. 7.5 abonelik ucundaki aynı sorunda bunu açıkça
//     reddetti ve doğru çözümü de yazdı: "gecikme eklemek değil, e-postayı
//     KUYRUĞA ALMAK".
//
//  2. KUYRUK (worker job) BURADA MÜMKÜN DEĞİL. 6.3'ün gönderim job'ı
//     çalışıyor çünkü mesajın içeriği veritabanından yeniden üretilebiliyor.
//     Sıfırlama linki üretilemez: token veritabanında HASH'li duruyor
//     (migration 00020) ve ham değeri yalnızca isteği karşılayan süreç
//     biliyor. Hash'i çözmek için tokenı ham saklamak gerekirdi — yani
//     kuyruk uğruna 1.2/6.4/8.3'ün token disiplinini bozmak.
//
//  3. O HÂLDE GÖNDERİM api'DE KALIYOR ve cevaptan AYRILIYOR: `Request`
//     çağıranı hiç bekletmiyor, işi kendi context'inde sürdürüyor
//     (3.2'nin `writeContext` deseni: `context.WithoutCancel` ile isteğin
//     iptalinden koparılmış, kendi zaman aşımı olan bir context). Cevap
//     süresi artık adresin kayıtlı olup olmamasından TAMAMEN bağımsız.
//
// Bedeli yazılı: süreç gönderim tamamlanmadan kapanırsa o e-posta kaybolur
// (kullanıcı yeniden ister). Karşılığında uç, gerçek bir sağlayıcı
// takıldığında bile — ki o zaman fark milisaniyeler değil YÜZ milisaniyeler
// olurdu — sızdırmaz kalıyor.
package passwordreset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/token"
)

var (
	// ErrTokenNotFound, token tanınmıyorsa veya çoktan kullanılmışsa döner.
	ErrTokenNotFound = errors.New("sıfırlama bağlantısı geçersiz")
	// ErrTokenExpired, token'ın süresi dolmuşsa döner.
	ErrTokenExpired = errors.New("sıfırlama bağlantısının süresi dolmuş")
)

const (
	// tokenTTL, sıfırlama bağlantısının ömrü.
	//
	// BİR SAAT — 8.3'ün yedi günlük davetinden çok daha dar, çünkü verdiği
	// yetenek çok daha ağır: davet "bir organizasyona üye ol", bu ise "bu
	// hesabın parolasını değiştir", yani doğrudan HESAP DEVRALMA.
	tokenTTL = time.Hour

	// backgroundTimeout, cevaptan sonra süren işin üst sınırı.
	//
	// İstek context'inden KOPARILMIŞ bir context kullanılıyor (aşağıya bkz.)
	// ama sınırsız değil: takılan bir gönderim goroutine'i sonsuza kadar
	// yaşamamalı.
	backgroundTimeout = 15 * time.Second
)

// Beginner, transaction başlatabilen bağlantı havuzu.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Mailer, sıfırlama linkini gönderen bağımlılık.
//
// 6.3'ün `notifications.Channel` arayüzünün ta kendisi (6.4 ve 8.3'ün
// kullandığıyla aynı): ikinci bir e-posta soyutlaması, gerçek sağlayıcıya
// geçişi dört ayrı yerde yapmak demekti.
type Mailer interface {
	Send(ctx context.Context, target string, msg notifications.Message) error
}

// Service, parola sıfırlama akışı.
type Service struct {
	db      Beginner
	queries *store.Queries
	hasher  auth.Hasher
	mailer  Mailer
	// publicBaseURL, e-postadaki linkin kökü — ARAYÜZÜN adresi (6.4'ün kararı).
	publicBaseURL string
	logger        *slog.Logger
}

// NewService, servisi kurar.
func NewService(
	db Beginner,
	queries *store.Queries,
	hasher auth.Hasher,
	mailer Mailer,
	publicBaseURL string,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		db:            db,
		queries:       queries,
		hasher:        hasher,
		mailer:        mailer,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
		logger:        logger,
	}
}

// Request, sıfırlama bağlantısı ister.
//
// ÇAĞIRANI HİÇ BEKLETMİYOR ve HİÇBİR KOŞULDA HATA DÖNDÜRMÜYOR: adresin
// kayıtlı olup olmadığı ne gövdeye ne de SÜREYE yansımalı (paket yorumuna
// bkz.). Çağıran her durumda aynı cevabı yazıyor.
//
// `ctx` YALNIZCA türetme için kullanılıyor; iş, isteğin iptalinden koparılmış
// kendi context'inde sürüyor — handler döndüğünde istek context'i iptal
// oluyor ve iş onunla birlikte ölürdü.
func (s *Service) Request(ctx context.Context, rawEmail string) {
	email := strings.ToLower(strings.TrimSpace(rawEmail))

	// 3.2'NİN DESENİ: `context.WithoutCancel` ile iptali koparıp kendi
	// zaman aşımını koymak. Orada gerekçe "ölçüm alındıysa yazma
	// düşürülmemeli"ydi; burada "cevap süresi işi beklememeli".
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	go func() {
		defer cancel()

		if err := s.issue(work, email); err != nil {
			// Kullanıcıya SÖYLENMİYOR (söylenecek bir cevap kalmadı, o çoktan
			// gönderildi) ama sessiz de kalmıyor: sıfırlama isteyip e-posta
			// alamayan kullanıcının sebebi burada görünür.
			s.logger.Error("parola sıfırlama isteği işlenemedi",
				slog.String("error", err.Error()))
		}
	}()
}

// issue, token'ı yazar ve linki gönderir.
func (s *Service) issue(ctx context.Context, email string) error {
	raw, err := token.New()
	if err != nil {
		return err
	}

	rows, err := s.queries.CreatePasswordResetToken(ctx, store.CreatePasswordResetTokenParams{
		TokenHash: token.Hash(raw),
		ExpiresAt: time.Now().Add(tokenTTL),
		Email:     email,
	})
	if err != nil {
		return fmt.Errorf("sıfırlama token'ı yazılamadı: %w", err)
	}

	if rows == 0 {
		// Adres kayıtlı değil. E-posta GÖNDERİLMİYOR — kayıtsız bir adrese
		// "birisi parolanızı sıfırlamak istedi" yazmak, o adresi ürünün
		// spam'ine dönüştürmek olurdu. Çağıran bu ayrımı zaten göremiyor.
		s.logger.Info("kayıtlı olmayan adres için sıfırlama isteği")

		return nil
	}

	return s.sendLink(ctx, email, raw)
}

// sendLink, sıfırlama e-postasını gönderir.
func (s *Service) sendLink(ctx context.Context, email, raw string) error {
	link := s.publicBaseURL + "/password-reset/confirm?token=" + raw

	body := fmt.Sprintf(
		"Parolanızı sıfırlamak için: %s\n\n"+
			"Bağlantı %d saat geçerli ve TEK KULLANIMLIK.\n"+
			"Bu isteği siz yapmadıysanız bir şey yapmanıza gerek yok; parolanız değişmedi.",
		link, int(tokenTTL.Hours()),
	)

	// İŞLEM E-POSTASI, kesinti bildirimi değil: konu satırına "Kesinti:"
	// öneki konmuyor (bkz. notifications.MessageKind — 8.3'ün düzeltmesi).
	//
	// `OrgName` YOK ve olamaz: parola sıfırlama bir organizasyondan ÖNCE
	// geliyor (kullanıcı hiçbir org'un üyesi olmayabilir). Ürünün adı
	// kullanılıyor.
	msg := notifications.Message{
		Kind:    notifications.MessageKindTransactional,
		OrgName: "Uptime Mini",
		Title:   "Parola sıfırlama",
		Body:    body,
	}

	if err := s.mailer.Send(ctx, email, msg); err != nil {
		return fmt.Errorf("sıfırlama e-postası gönderilemedi: %w", err)
	}

	return nil
}

// Confirm, token ile parolayı değiştirir.
//
// ÜÇ YAZMA TEK TRANSACTION'DA: yeni parola · kullanıcının TÜM sıfırlama
// token'ları · TÜM refresh oturumları. Araya giren bir hata, "parola
// değişti ama eski oturumlar açık" ya da "oturumlar düştü ama parola eski"
// gibi kendiyle çelişen bir durum bırakırdı (4.2'nin "timeline ve durum tek
// yazmada" kararının aynı gerekçesi).
func (s *Service) Confirm(ctx context.Context, rawToken, newPassword string) error {
	row, err := s.queries.GetPasswordResetToken(ctx, token.Hash(rawToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Tanınmıyor VEYA kullanılmış (kullanımda satır siliniyor):
			// ikisi de aynı cevap.
			return ErrTokenNotFound
		}

		return fmt.Errorf("sıfırlama token'ı okunamadı: %w", err)
	}

	if time.Now().After(row.ExpiresAt) {
		return ErrTokenExpired
	}

	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return fmt.Errorf("parola hash'lenemedi: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	updated, err := q.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{
		ID:           row.UserID,
		PasswordHash: hash,
	})
	if err != nil {
		return fmt.Errorf("parola güncellenemedi: %w", err)
	}
	if updated == 0 {
		// Kullanıcı token ile satır arasında silinmiş olabilir.
		return ErrTokenNotFound
	}

	// TÜM sıfırlama token'ları düşüyor, yalnızca kullanılan değil: saldırgan
	// da bir sıfırlama isteyip token'ını beklemede tutmuş olabilir.
	if _, err := q.DeletePasswordResetTokensForUser(ctx, row.UserID); err != nil {
		return fmt.Errorf("sıfırlama token'ları silinemedi: %w", err)
	}

	// TÜM REFRESH OTURUMLARI İPTAL (1.2'nin iptal mekanizması).
	//
	// Parolasını sıfırlayan kişinin gerekçesi çoğu zaman hesabının ele
	// geçirilmiş olması. İptal edilmeseydi saldırganın açık oturumu, parola
	// değişmesine rağmen refresh token ömrü (30 gün) boyunca yaşamaya devam
	// ederdi — sıfırlamanın anlamını ortadan kaldıran bir açık.
	revoked, err := q.RevokeAllUserSessions(ctx, row.UserID)
	if err != nil {
		return fmt.Errorf("oturumlar iptal edilemedi: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("parola sıfırlama kaydedilemedi: %w", err)
	}

	s.logger.Info("parola sıfırlandı",
		slog.String("user_id", row.UserID.String()),
		slog.Int64("iptal_edilen_oturum", revoked))

	return nil
}
