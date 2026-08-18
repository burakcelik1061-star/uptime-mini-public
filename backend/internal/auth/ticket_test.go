package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
)

func TestTicketIssueAndVerify(t *testing.T) {
	t.Parallel()

	service := auth.NewTicketService([]byte("bilet-sirri-bilet-sirri-bilet-sirri"), auth.DefaultTicketTTL)

	userID, orgID := uuid.New(), uuid.New()

	ticket, err := service.Issue(userID, orgID)
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	if ticket.ExpiresIn != int64(auth.DefaultTicketTTL.Seconds()) {
		t.Errorf("ExpiresIn = %d, beklenen %d", ticket.ExpiresIn, int64(auth.DefaultTicketTTL.Seconds()))
	}

	identity, err := service.Verify(ticket.Value)
	if err != nil {
		t.Fatalf("bilet doğrulanamadı: %v", err)
	}

	if identity.UserID != userID {
		t.Errorf("UserID = %s, beklenen %s", identity.UserID, userID)
	}

	// KAPSAM BİLETİN İÇİNDE: el sıkışma org kimliğini istemciden değil buradan
	// alıyor (bkz. ws.go).
	if identity.OrgID != orgID {
		t.Errorf("OrgID = %s, beklenen %s", identity.OrgID, orgID)
	}
}

// Biletin en kritik özelliği: İKİNCİ KEZ KULLANILAMAMASI. Sorgu parametresinde
// taşındığı için erişim loguna yazılıyor; loga düşen dizenin değersiz olması
// tam olarak bu davranışa dayanıyor.
func TestTicketIsSingleUse(t *testing.T) {
	t.Parallel()

	service := auth.NewTicketService([]byte("bilet-sirri-bilet-sirri-bilet-sirri"), time.Minute)

	ticket, err := service.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	if _, err := service.Verify(ticket.Value); err != nil {
		t.Fatalf("ilk doğrulama başarısız: %v", err)
	}

	_, err = service.Verify(ticket.Value)
	if !errors.Is(err, auth.ErrTicketUsed) {
		t.Fatalf("ikinci doğrulama hatası = %v, beklenen ErrTicketUsed", err)
	}
}

// Aynı kullanıcı için üretilen iki bilet birbirini geçersiz kılmamalı: jti
// bilet başına üretiliyor, kullanıcı başına değil.
func TestTicketsAreIndependent(t *testing.T) {
	t.Parallel()

	service := auth.NewTicketService([]byte("bilet-sirri-bilet-sirri-bilet-sirri"), time.Minute)
	userID, orgID := uuid.New(), uuid.New()

	first, err := service.Issue(userID, orgID)
	if err != nil {
		t.Fatalf("ilk bilet üretilemedi: %v", err)
	}

	second, err := service.Issue(userID, orgID)
	if err != nil {
		t.Fatalf("ikinci bilet üretilemedi: %v", err)
	}

	if first.Value == second.Value {
		t.Fatal("iki bilet birebir aynı dizeye düştü")
	}

	if _, err := service.Verify(first.Value); err != nil {
		t.Fatalf("ilk bilet doğrulanamadı: %v", err)
	}

	if _, err := service.Verify(second.Value); err != nil {
		t.Fatalf("ikinci bilet doğrulanamadı: %v", err)
	}
}

func TestTicketExpires(t *testing.T) {
	t.Parallel()

	// TTL'i milisaniyeye indirmek gerçek zamanı beklemeden süre dolmasını
	// ölçmenin en ucuz yolu.
	service := auth.NewTicketService([]byte("bilet-sirri-bilet-sirri-bilet-sirri"), time.Millisecond)

	ticket, err := service.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	_, err = service.Verify(ticket.Value)
	if !errors.Is(err, auth.ErrTicketExpired) {
		t.Fatalf("hata = %v, beklenen ErrTicketExpired", err)
	}
}

// AYNI SIR, FARKLI AUDIENCE: access token bilet yerine, bilet de access token
// yerine kullanılamamalı. İki servisin aynı imzalama anahtarını paylaşması bu
// yüzden güvenli.
func TestAccessTokenIsNotAcceptedAsTicket(t *testing.T) {
	t.Parallel()

	secret := []byte("ortak-sir-ortak-sir-ortak-sir-1234")

	tokens := auth.NewTokenService(secret, time.Hour)
	tickets := auth.NewTicketService(secret, time.Minute)

	access, err := tokens.Issue(uuid.New())
	if err != nil {
		t.Fatalf("access token üretilemedi: %v", err)
	}

	if _, err := tickets.Verify(access.Value); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("access token bilet olarak kabul edildi (hata: %v)", err)
	}

	ticket, err := tickets.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	if _, err := tokens.Verify(ticket.Value); err == nil {
		t.Fatal("bilet access token olarak kabul edildi")
	}
}

func TestTicketRejectsTamperedAndForeignSignatures(t *testing.T) {
	t.Parallel()

	service := auth.NewTicketService([]byte("bilet-sirri-bilet-sirri-bilet-sirri"), time.Minute)
	other := auth.NewTicketService([]byte("baska-sir-baska-sir-baska-sir-9876"), time.Minute)

	ticket, err := other.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	if _, err := service.Verify(ticket.Value); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("başka sırla imzalı bilet kabul edildi (hata: %v)", err)
	}

	mine, err := service.Issue(uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("bilet üretilemedi: %v", err)
	}

	// Gövdeyi kurcalamak imzayı bozar.
	tampered := strings.Replace(mine.Value, ".", ".x", 1)
	if _, err := service.Verify(tampered); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("kurcalanmış bilet kabul edildi (hata: %v)", err)
	}

	if _, err := service.Verify(""); !errors.Is(err, auth.ErrTicketInvalid) {
		t.Fatalf("boş bilet kabul edildi (hata: %v)", err)
	}
}
