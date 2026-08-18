package passwordreset

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/token"
)

// PAROLA SIFIRLAMA (8.4) — gerçek veritabanına karşı.
//
// Neden gerçek DB: akışın iki garantisi tamamen transaction ve sorgu
// davranışında (üç yazma tek transaction, TÜM oturumların iptali). Sahte bir
// store ile ölçülseydi test kendi taklidini doğrulamış olurdu (1.2'nin kuralı).
//
// TESTLER `issue`'YU DOĞRUDAN ÇAĞIRIYOR, `Request`i değil: `Request` işi
// bilinçli olarak bir goroutine'e veriyor (cevap süresi sızmasın) ve
// goroutine'in bitmesini beklemek testi zamana bağlı kılardı. `Request`in
// KENDİ sözleşmesi (bekletmemek) ayrıca ölçülüyor — aşağıdaki son teste bkz.

// recordingMailer, gönderilen sıfırlama e-postalarını tutan sahte adapter.
//
// KİLİTLİ ve bu zorunlu: `Request` gönderimi bilinçli olarak BAŞKA BİR
// GOROUTINE'e veriyor (cevap süresi sızmasın diye — paket yorumuna bkz.),
// yani bu sahteye test goroutine'i ile gönderim goroutine'i AYNI ANDA
// dokunuyor. Kilitsiz hâli gerçek bir veri yarışıydı ve `go test -race`
// (CI'ın integration job'ı) onu yakaladı.
//
// Yarış SAHTEDEYDİ, üründe değil: üretimdeki adapter'lar durum tutmuyor.
type recordingMailer struct {
	mu      sync.Mutex
	targets []string
	bodies  []string
}

func (m *recordingMailer) Send(_ context.Context, target string, msg notifications.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.targets = append(m.targets, target)
	m.bodies = append(m.bodies, msg.Body)

	return nil
}

// sent, gönderilen adreslerin KOPYASINI döner.
//
// Çağıran doğrudan `targets`e bakamaz: okuma da kilit altında olmalı.
func (m *recordingMailer) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.targets...)
}

// lastBody, son gönderilen gövdeyi döner (kilit altında).
func (m *recordingMailer) lastBody() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.bodies) == 0 {
		return ""
	}

	return m.bodies[len(m.bodies)-1]
}

type fixture struct {
	pool    *pgxpool.Pool
	queries *store.Queries
	svc     *Service
	mailer  *recordingMailer
	hasher  auth.Hasher
}

// testHasher, ucuz Argon2 parametreleri (apitest'teki gerekçenin aynısı).
func testHasher() auth.Hasher {
	return auth.Hasher{Params: auth.Argon2Params{
		MemoryKiB: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}}
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	pool := testdb.Pool(t, "passwordreset")
	queries := store.New(pool)
	mailer := &recordingMailer{}
	hasher := testHasher()

	svc := NewService(pool, queries, hasher, mailer, "http://localhost:3100",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	return fixture{pool: pool, queries: queries, svc: svc, mailer: mailer, hasher: hasher}
}

// createUser, parolası bilinen bir kullanıcı açar.
func (f fixture) createUser(t *testing.T, email, password string) uuid.UUID {
	t.Helper()

	hash, err := f.hasher.Hash(password)
	if err != nil {
		t.Fatalf("parola hash'lenemedi: %v", err)
	}

	user, err := f.queries.CreateUser(t.Context(), store.CreateUserParams{
		Email:        email,
		PasswordHash: hash,
		Name:         "Test Kullanıcı",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	return user.ID
}

// lastToken, son gönderilen e-postadaki ham token'ı çıkarır.
//
// Token'ı testin içinden üretmek yerine E-POSTADAN okumak bilinçli: ölçülen
// şey "servis ne yazdı" değil, KULLANICININ ELİNE NE GEÇTİĞİ.
func (f fixture) lastToken(t *testing.T) string {
	t.Helper()

	body := f.mailer.lastBody()
	if body == "" {
		t.Fatal("sıfırlama e-postası gönderilmemiş")
	}

	_, after, found := strings.Cut(body, "token=")
	if !found {
		t.Fatalf("gövdede token yok: %q", body)
	}

	return strings.Fields(after)[0]
}

func TestIssueSendsLinkAndStoresHashedToken(t *testing.T) {
	f := newFixture(t)
	userID := f.createUser(t, "unutkan@example.com", "eski-parola-123")

	if err := f.svc.issue(t.Context(), "unutkan@example.com"); err != nil {
		t.Fatalf("sıfırlama isteği başarısız: %v", err)
	}

	if sent := f.mailer.sent(); len(sent) != 1 || sent[0] != "unutkan@example.com" {
		t.Fatalf("gönderim = %v, beklenen tek ve doğru adres", sent)
	}

	raw := f.lastToken(t)

	// TOKEN VERİTABANINDA HAM DURMUYOR (1.2/6.4/8.3'ün kararı).
	row, err := f.queries.GetPasswordResetToken(t.Context(), token.Hash(raw))
	if err != nil {
		t.Fatalf("hash'li token ile kayıt bulunamadı: %v", err)
	}
	if row.UserID != userID {
		t.Errorf("token başka kullanıcıya yazılmış")
	}

	if _, err := f.queries.GetPasswordResetToken(t.Context(), raw); err == nil {
		t.Fatal("HAM token ile kayıt bulundu — token hash'lenmeden saklanıyor")
	}
}

// KAYITSIZ ADRES: token YAZILMIYOR ve e-posta GÖNDERİLMİYOR — ama çağıran
// hiçbir fark görmüyor (hata da dönmüyor).
func TestIssueIsSilentForUnknownAddress(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.issue(t.Context(), "kimse@example.com"); err != nil {
		t.Fatalf("bilinmeyen adres hata döndürdü: %v", err)
	}

	if sent := f.mailer.sent(); len(sent) != 0 {
		t.Errorf("kayıtsız adrese e-posta gönderildi: %v", sent)
	}

	var count int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM password_reset_tokens`).Scan(&count); err != nil {
		t.Fatalf("sayım yapılamadı: %v", err)
	}
	if count != 0 {
		t.Errorf("kayıtsız adres için %d token yazıldı", count)
	}
}

// BU ADIMIN EN ÖNEMLİ TESTİ: başarılı sıfırlama TÜM refresh oturumlarını
// iptal ediyor.
//
// Parolasını sıfırlayan kişinin gerekçesi çoğu zaman hesabının ele
// geçirilmiş olması; iptal edilmeseydi saldırganın açık oturumu parola
// değişse bile refresh TTL'i (30 gün) boyunca yaşardı.
func TestConfirmRevokesAllRefreshSessions(t *testing.T) {
	f := newFixture(t)
	userID := f.createUser(t, "ele-gecmis@example.com", "eski-parola-123")

	// İki AYRI oturum ailesi (iki cihaz / saldırgan + kullanıcı).
	for range 2 {
		if _, err := f.queries.CreateRefreshToken(t.Context(), store.CreateRefreshTokenParams{
			SessionID: uuid.New(),
			UserID:    userID,
			TokenHash: []byte(uuid.NewString()),
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
		}); err != nil {
			t.Fatalf("refresh token yazılamadı: %v", err)
		}
	}

	if err := f.svc.issue(t.Context(), "ele-gecmis@example.com"); err != nil {
		t.Fatalf("sıfırlama isteği başarısız: %v", err)
	}

	if err := f.svc.Confirm(t.Context(), f.lastToken(t), "yeni-parola-456"); err != nil {
		t.Fatalf("sıfırlama başarısız: %v", err)
	}

	var open int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`,
		userID).Scan(&open); err != nil {
		t.Fatalf("sayım yapılamadı: %v", err)
	}
	if open != 0 {
		t.Errorf("sıfırlamadan sonra %d oturum AÇIK kaldı", open)
	}
}

func TestConfirmChangesPasswordSoNewOneVerifies(t *testing.T) {
	f := newFixture(t)
	userID := f.createUser(t, "degisen@example.com", "eski-parola-123")

	if err := f.svc.issue(t.Context(), "degisen@example.com"); err != nil {
		t.Fatalf("sıfırlama isteği başarısız: %v", err)
	}

	if err := f.svc.Confirm(t.Context(), f.lastToken(t), "yeni-parola-456"); err != nil {
		t.Fatalf("sıfırlama başarısız: %v", err)
	}

	user, err := f.queries.GetUserByID(t.Context(), userID)
	if err != nil {
		t.Fatalf("kullanıcı okunamadı: %v", err)
	}

	if err := f.hasher.Verify(user.PasswordHash, "yeni-parola-456"); err != nil {
		t.Errorf("yeni parola doğrulanmadı: %v", err)
	}

	if err := f.hasher.Verify(user.PasswordHash, "eski-parola-123"); err == nil {
		t.Error("ESKİ parola hâlâ geçerli")
	}
}

// TEK KULLANIMLIK: aynı bağlantı ikinci kez çalışmamalı.
func TestConfirmIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.createUser(t, "tek@example.com", "eski-parola-123")

	if err := f.svc.issue(t.Context(), "tek@example.com"); err != nil {
		t.Fatalf("sıfırlama isteği başarısız: %v", err)
	}

	raw := f.lastToken(t)

	if err := f.svc.Confirm(t.Context(), raw, "yeni-parola-456"); err != nil {
		t.Fatalf("ilk sıfırlama başarısız: %v", err)
	}

	if err := f.svc.Confirm(t.Context(), raw, "baska-parola-789"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("ikinci kullanım hatası = %v, beklenen ErrTokenNotFound", err)
	}
}

// BAŞARILI SIFIRLAMA, BEKLEYEN DİĞER TOKEN'LARI DA ÖLDÜRÜYOR.
//
// Saldırgan da bir sıfırlama isteyip token'ını beklemede tutabilir; meşru
// kullanıcı parolasını değiştirdiğinde o token da geçersiz olmalı.
func TestConfirmInvalidatesOtherOutstandingTokens(t *testing.T) {
	f := newFixture(t)
	f.createUser(t, "iki-token@example.com", "eski-parola-123")

	// Saldırganın isteği.
	if err := f.svc.issue(t.Context(), "iki-token@example.com"); err != nil {
		t.Fatalf("birinci istek başarısız: %v", err)
	}
	attackerToken := f.lastToken(t)

	// Kullanıcının kendi isteği.
	if err := f.svc.issue(t.Context(), "iki-token@example.com"); err != nil {
		t.Fatalf("ikinci istek başarısız: %v", err)
	}
	userToken := f.lastToken(t)

	if err := f.svc.Confirm(t.Context(), userToken, "yeni-parola-456"); err != nil {
		t.Fatalf("sıfırlama başarısız: %v", err)
	}

	if err := f.svc.Confirm(t.Context(), attackerToken, "saldirgan-parola"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("bekleyen diğer token hâlâ geçerli: %v", err)
	}
}

func TestConfirmRejectsExpiredToken(t *testing.T) {
	f := newFixture(t)
	f.createUser(t, "gec@example.com", "eski-parola-123")

	if err := f.svc.issue(t.Context(), "gec@example.com"); err != nil {
		t.Fatalf("sıfırlama isteği başarısız: %v", err)
	}

	// Süre GEÇMİŞE çekiliyor: bir saat beklemek mümkün değil ve saati
	// sahtelemek bu tek test için taşınacak bir soyutlama olurdu (8.3'ün
	// süresi dolmuş davet testiyle aynı yaklaşım).
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE password_reset_tokens SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("süre geçmişe çekilemedi: %v", err)
	}

	if err := f.svc.Confirm(t.Context(), f.lastToken(t), "yeni-parola-456"); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("hata = %v, beklenen ErrTokenExpired", err)
	}
}

func TestConfirmRejectsUnknownToken(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Confirm(t.Context(), "uydurma-token", "yeni-parola-456"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("hata = %v, beklenen ErrTokenNotFound", err)
	}
}

// `Request` ÇAĞIRANI BEKLETMİYOR — bu paketin belirleyici kararı.
//
// Test süreye bakıyor ama YARIŞA DAYALI DEĞİL: ölçtüğü şey "işin tamamı bu
// sürede biter mi" değil, "Request işi BEKLEMEDEN dönüyor mu". Eşik bilinçli
// olarak cömert; asıl kanıt, gönderimin çağrı DÖNDÜKTEN SONRA gerçekleşmesi.
func TestRequestDoesNotBlockOnWork(t *testing.T) {
	f := newFixture(t)
	f.createUser(t, "hizli@example.com", "eski-parola-123")

	start := time.Now()
	f.svc.Request(t.Context(), "hizli@example.com")
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Errorf("Request %s bekletti; iş cevaptan ayrılmamış", elapsed)
	}

	// İş gerçekten YAPILIYOR: goroutine'in bitmesini bekliyoruz.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.mailer.sent()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("Request'in başlattığı iş e-posta göndermedi")
}
