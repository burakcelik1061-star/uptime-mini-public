package sessions

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu paketin testleri GERÇEK bir Postgres'e karşı koşuyor (kurulum:
// internal/testdb).
//
// Gerekçe: burada ölçülen şey (satır kilidi, transaction sınırları, eş
// zamanlı iki yenilemeden yalnızca birinin kazanması) sahte bir store ile
// ölçülemez — sahte store'a bu davranışı ben yazardım ve test kendi
// kopyasını doğrulamış olurdu.

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return testdb.Pool(t, "sessions")
}

const testRefreshSecret = "test-refresh-anahtari-32-karakterden-uzun"

func newTestService(t *testing.T, pool *pgxpool.Pool, ttl time.Duration) *Service {
	t.Helper()

	return NewService(pool, store.New(pool),
		auth.NewRefreshTokenFactory([]byte(testRefreshSecret), ttl),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// createUser, oturumların bağlanacağı kullanıcıyı yaratır (refresh_tokens
// users'a foreign key ile bağlı).
func createUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()

	user, err := store.New(pool).CreateUser(t.Context(), store.CreateUserParams{
		Email:        "oturum-" + uuid.NewString() + "@example.com",
		PasswordHash: "$argon2id$test",
		Name:         "Test Kullanıcı",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	return user.ID
}

func countTokens(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) (total, revoked int) {
	t.Helper()

	row := pool.QueryRow(t.Context(),
		`SELECT count(*), count(revoked_at) FROM refresh_tokens WHERE session_id = $1`, sessionID)
	if err := row.Scan(&total, &revoked); err != nil {
		t.Fatalf("sayım yapılamadı: %v", err)
	}

	return total, revoked
}

func TestStartCreatesStoredSession(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)
	userID := createUser(t, pool)

	issued, err := svc.Start(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if issued.Token.Value == "" {
		t.Fatal("token değeri boş")
	}

	var (
		storedHash []byte
		usedAt     *time.Time
		revokedAt  *time.Time
	)
	row := pool.QueryRow(t.Context(),
		`SELECT token_hash, used_at, revoked_at FROM refresh_tokens WHERE session_id = $1`, issued.SessionID)
	if err := row.Scan(&storedHash, &usedAt, &revokedAt); err != nil {
		t.Fatalf("satır okunamadı: %v", err)
	}

	// HAM TOKEN veritabanında yok, yalnızca anahtarlı özeti var.
	if string(storedHash) == issued.Token.Value {
		t.Error("ham token saklanmış")
	}
	if usedAt != nil || revokedAt != nil {
		t.Errorf("yeni token kullanılmış/iptal görünüyor: used=%v revoked=%v", usedAt, revokedAt)
	}
}

func TestRotateIssuesNewTokenInSameSession(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)
	userID := createUser(t, pool)

	issued, err := svc.Start(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	rotated, err := svc.Rotate(t.Context(), issued.Token.Value)
	if err != nil {
		t.Fatalf("yenileme başarısız: %v", err)
	}

	if rotated.Token.Value == issued.Token.Value {
		t.Error("yenilemede aynı token geri döndü — rotasyon yok")
	}
	if rotated.SessionID != issued.SessionID {
		t.Errorf("session değişti: %s → %s", issued.SessionID, rotated.SessionID)
	}
	if rotated.UserID != userID {
		t.Errorf("userID = %s, beklenen %s", rotated.UserID, userID)
	}

	total, revoked := countTokens(t, pool, issued.SessionID)
	if total != 2 {
		t.Errorf("oturumdaki token sayısı = %d, beklenen 2", total)
	}
	if revoked != 0 {
		t.Errorf("iptal edilmiş token sayısı = %d, beklenen 0", revoked)
	}

	// Zincir devam edebilmeli.
	if _, err := svc.Rotate(t.Context(), rotated.Token.Value); err != nil {
		t.Errorf("ikinci yenileme başarısız: %v", err)
	}
}

func TestRotateRejectsUnknownToken(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)

	if _, err := svc.Rotate(t.Context(), "boyle-bir-token-yok"); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("hata = %v, beklenen ErrInvalidRefreshToken", err)
	}
}

func TestRotateRejectsExpiredToken(t *testing.T) {
	pool := newTestPool(t)
	// Süresi geçmiş doğan token.
	svc := newTestService(t, pool, -time.Minute)
	userID := createUser(t, pool)

	issued, err := svc.Start(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if _, err := svc.Rotate(t.Context(), issued.Token.Value); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("hata = %v, beklenen ErrInvalidRefreshToken", err)
	}
}

// Bu adımın asıl konusu: kullanılmış bir token tekrar sunulduğunda oturumun
// TAMAMI iptal edilmeli. Çalınan token'ın saldırgan tarafından kullanılması
// hâlinde meşru kullanıcının zinciri de düşüyor — ikisini ayırt etmek
// mümkün değil, güvenli taraf bu.
func TestRotateDetectsReuseAndRevokesWholeSession(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)
	userID := createUser(t, pool)

	issued, err := svc.Start(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	rotated, err := svc.Rotate(t.Context(), issued.Token.Value)
	if err != nil {
		t.Fatalf("yenileme başarısız: %v", err)
	}

	// ESKİ token yeniden sunuluyor.
	if _, err := svc.Rotate(t.Context(), issued.Token.Value); !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("hata = %v, beklenen ErrRefreshTokenReused", err)
	}

	total, revoked := countTokens(t, pool, issued.SessionID)
	if total != revoked {
		t.Errorf("oturumdaki %d token'ın yalnızca %d tanesi iptal edilmiş", total, revoked)
	}

	// Geçerli olan (çalınmamış) token da artık çalışmamalı.
	if _, err := svc.Rotate(t.Context(), rotated.Token.Value); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("iptal edilmiş oturumun token'ı kabul edildi (hata = %v)", err)
	}
}

// Aynı token'la eş zamanlı iki yenileme: satır kilidi sayesinde yalnızca
// biri kazanmalı, diğeri yeniden kullanım olarak işlenmeli. Kilit olmasaydı
// ikisi de geçerli sayılır ve aynı aileden iki paralel zincir doğardı.
func TestConcurrentRotationLetsOnlyOneWin(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)
	userID := createUser(t, pool)

	issued, err := svc.Start(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		reused  int
		other   []error
	)

	start := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			_, err := svc.Rotate(t.Context(), issued.Token.Value)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrRefreshTokenReused):
				reused++
			default:
				other = append(other, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("beklenmeyen hatalar: %v", other)
	}
	if success != 1 || reused != 1 {
		t.Fatalf("başarılı = %d, yeniden kullanım = %d; beklenen 1 ve 1", success, reused)
	}
}

func TestRevokeClosesWholeSession(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)
	userID := createUser(t, pool)

	issued, err := svc.Start(t.Context(), userID)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	rotated, err := svc.Rotate(t.Context(), issued.Token.Value)
	if err != nil {
		t.Fatalf("yenileme başarısız: %v", err)
	}

	if err := svc.Revoke(t.Context(), rotated.Token.Value); err != nil {
		t.Fatalf("iptal başarısız: %v", err)
	}

	total, revoked := countTokens(t, pool, issued.SessionID)
	if total != revoked {
		t.Errorf("oturumdaki %d token'ın yalnızca %d tanesi iptal edilmiş", total, revoked)
	}

	if _, err := svc.Rotate(t.Context(), rotated.Token.Value); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("logout sonrası token kabul edildi (hata = %v)", err)
	}
}

// Logout idempotent: bilinmeyen token da başarılı sayılıyor (etki aynı,
// ayrıca "bu token sistemde var mı" bilgisi sızmıyor).
func TestRevokeUnknownTokenIsNoOp(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)

	if err := svc.Revoke(t.Context(), "boyle-bir-token-yok"); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
}

// Bir kullanıcının oturumları başka kullanıcıya sızmamalı: iki ayrı
// kullanıcının oturumları birbirinden bağımsız.
func TestSessionsAreIsolatedPerUser(t *testing.T) {
	pool := newTestPool(t)
	svc := newTestService(t, pool, time.Hour)

	first := createUser(t, pool)
	second := createUser(t, pool)

	firstSession, err := svc.Start(t.Context(), first)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	secondSession, err := svc.Start(t.Context(), second)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if err := svc.Revoke(t.Context(), firstSession.Token.Value); err != nil {
		t.Fatalf("iptal başarısız: %v", err)
	}

	rotated, err := svc.Rotate(t.Context(), secondSession.Token.Value)
	if err != nil {
		t.Fatalf("ikinci kullanıcının oturumu etkilendi: %v", err)
	}
	if rotated.UserID != second {
		t.Errorf("userID = %s, beklenen %s", rotated.UserID, second)
	}
}
