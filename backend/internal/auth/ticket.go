package auth

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// WebSocket bileti (Faz 6.1).
//
// NEDEN AYRI BİR TOKEN TÜRÜ VAR: tarayıcının `WebSocket` API'si isteğe
// `Authorization` başlığı EKLEYEMİYOR (fetch'in aksine başlık parametresi
// yok). Access token 2.1'den beri yalnızca bellekte, refresh cookie'sinin
// Path'i de `/v1/auth` — yani `/v1/ws`e giden el sıkışmada hiçbir kimlik
// bilgisi kendiliğinden taşınmıyor.
//
// Çözüm: istemci NORMAL bir istekle (Authorization başlığıyla, org üyeliği
// veritabanından doğrulanmış bir uçtan) kısa ömürlü bir bilet alıyor ve onu
// el sıkışmanın sorgu parametresinde taşıyor.
//
// SORGU PARAMETRESİ ERİŞİM LOGUNA YAZILIR — bu bilinen bedel ve biletin
// tasarımı tam da bunun için:
//
//   - ömrü 30 saniye (access token'ın 15 dakikasının yanında ihmal edilebilir
//     bir pencere),
//   - TEK KULLANIMLIK: doğrulandığı anda jti'si yakılıyor, ikinci kez
//     sunulduğunda reddediliyor,
//   - kapsamı tek bir organizasyon: bilet başka bir org'un kanalını açamaz.
//
// Yani loga düşen dize, düştüğü anda çoktan değersiz.
//
// TEK KULLANIM KAYDI BELLEKTE: bu kurulumda tek bir api süreci var ve bileti
// üreten süreç ile tüketen süreç aynı. Çok kopyalı bir api'de kayıt
// paylaşılmadığı için garanti "tek kullanım"dan "30 saniyelik TTL"e düşer —
// bilinçli bir sınır, ortak bir durum deposu (Redis vb.) eklemek 0.2'nin
// "kullanılmayan altyapı taşımıyoruz" kararını bozardı.
const (
	// ticketAudience, biletin NEREDE geçerli olduğunu sabitler. Access
	// token'ınkinden (`uptime-mini-api`) farklı: aynı sırla imzalansalar bile
	// bir access token WebSocket bileti yerine, bir bilet de access token
	// yerine KULLANILAMAZ.
	ticketAudience = "uptime-mini-ws"

	// tokenUseWSTicket, gövdedeki tür alanı — ikinci savunma (bkz. token.go).
	tokenUseWSTicket = "ws_ticket"

	// DefaultTicketTTL, biletin ömrü. El sıkışma bileti aldıktan hemen sonra
	// yapılıyor; 30 saniye yavaş bir ağ için bile fazlasıyla yeterli ve
	// sızması hâlinde kullanılabilir pencereyi minimumda tutuyor.
	DefaultTicketTTL = 30 * time.Second
)

var (
	// ErrTicketInvalid, bilet çözümlenemediğinde/imzası tutmadığında döner.
	ErrTicketInvalid = errors.New("bilet geçersiz")
	// ErrTicketExpired, biletin süresi dolduğunda döner.
	ErrTicketExpired = errors.New("bilet süresi dolmuş")
	// ErrTicketUsed, aynı bilet ikinci kez sunulduğunda döner. Ayrı bir hata:
	// "süresi dolmuş" ile karıştırılırsa, tekrar oynatma denemesi normal bir
	// zaman aşımı gibi görünürdü.
	ErrTicketUsed = errors.New("bilet zaten kullanılmış")
)

// Ticket, istemciye dönen bilet.
type Ticket struct {
	Value     string
	ExpiresAt time.Time
	// ExpiresIn, saniye cinsinden ömür (istemci kendi saatine güvenmeden
	// "biletim bayatladı mı" diyebilsin).
	ExpiresIn int64
}

// TicketIdentity, doğrulanmış bir biletin içeriği.
//
// OrgID de burada: bilet TEK BİR organizasyona kapsanmış durumda ve bağlantı
// yalnızca o org'un kanalını dinleyebiliyor. Kapsam biletin İÇİNDE çünkü
// üyelik denetimi bileti veren uçta (org-scoped alt-router, `requireOrgMembership`)
// veritabanına karşı ZATEN yapıldı — el sıkışmada ikinci kez sorulsaydı, o
// denetimin org ağacının dışında bir kopyası doğardı.
type TicketIdentity struct {
	UserID uuid.UUID
	OrgID  uuid.UUID
}

// ticketClaims, biletin gövdesi.
type ticketClaims struct {
	TokenUse string `json:"token_use"`
	OrgID    string `json:"org_id"`
	jwt.RegisteredClaims
}

// TicketService, WebSocket biletleri üretir ve TEK KULLANIMLIK doğrular.
type TicketService struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time

	// mu/used, kullanılmış biletlerin jti kaydı. Harita yalnızca ömrü dolmamış
	// biletleri tutuyor: her doğrulamada süresi geçmiş kayıtlar siliniyor, yani
	// 30 saniyelik bir pencerede açılan bağlantı sayısı kadar büyüyor.
	mu   sync.Mutex
	used map[string]time.Time
}

// NewTicketService, verilen imzalama anahtarı ve ömürle bir servis kurar.
//
// Anahtar access token'ınkiyle AYNI olabilir: iki token'ı ayıran şey sır değil
// audience + token_use kontrolleri (bkz. ticketAudience). Ayrı bir sır eklemek
// yapılandırmaya yönetilmesi gereken üçüncü bir değer koyardı.
func NewTicketService(secret []byte, ttl time.Duration) *TicketService {
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}

	return &TicketService{
		secret: secret,
		ttl:    ttl,
		now:    time.Now,
		used:   make(map[string]time.Time),
	}
}

// Issue, kullanıcı + organizasyon çifti için imzalı bir bilet üretir.
func (s *TicketService) Issue(userID, orgID uuid.UUID) (Ticket, error) {
	now := s.now()
	expiresAt := now.Add(s.ttl)

	claims := ticketClaims{
		TokenUse: tokenUseWSTicket,
		OrgID:    orgID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   tokenIssuer,
			Audience: jwt.ClaimStrings{ticketAudience},
			Subject:  userID.String(),
			// jti TEK KULLANIM garantisinin dayanağı: aynı kullanıcı aynı
			// saniyede iki bilet alsa bile ikisi ayrı kimliğe sahip, yani
			// birinin yakılması diğerini geçersiz kılmıyor.
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return Ticket{}, fmt.Errorf("bilet imzalanamadı: %w", err)
	}

	return Ticket{
		Value:     signed,
		ExpiresAt: expiresAt,
		ExpiresIn: int64(s.ttl.Seconds()),
	}, nil
}

// Verify, bileti doğrular VE YAKAR: aynı bilet ikinci kez kabul edilmez.
func (s *TicketService) Verify(raw string) (TicketIdentity, error) {
	parser := jwt.NewParser(
		// Algoritma listesi tek elemanlı — alg confusion saldırısı doğrulama
		// başlamadan biter (bkz. token.go'daki aynı not).
		jwt.WithValidMethods([]string{signingMethodHS256}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithAudience(ticketAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(0),
		jwt.WithTimeFunc(s.now),
	)

	var claims ticketClaims
	if _, err := parser.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return s.secret, nil
	}); err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return TicketIdentity{}, ErrTicketExpired
		}

		return TicketIdentity{}, fmt.Errorf("%w: %s", ErrTicketInvalid, err)
	}

	if claims.TokenUse != tokenUseWSTicket {
		return TicketIdentity{}, fmt.Errorf("%w: token türü %q", ErrTicketInvalid, claims.TokenUse)
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return TicketIdentity{}, fmt.Errorf("%w: sub bir uuid değil", ErrTicketInvalid)
	}

	orgID, err := uuid.Parse(claims.OrgID)
	if err != nil {
		return TicketIdentity{}, fmt.Errorf("%w: org_id bir uuid değil", ErrTicketInvalid)
	}

	if claims.ID == "" {
		return TicketIdentity{}, fmt.Errorf("%w: jti yok", ErrTicketInvalid)
	}

	if err := s.burn(claims.ID, claims.ExpiresAt.Time); err != nil {
		return TicketIdentity{}, err
	}

	return TicketIdentity{UserID: userID, OrgID: orgID}, nil
}

// burn, bileti kullanılmış olarak işaretler; ikinci çağrıda hata döner.
func (s *TicketService) burn(id string, expiresAt time.Time) error {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Süresi geçmiş kayıtlar temizleniyor: zaten süre kontrolünden geçemeyecek
	// biletlerin jti'sini saklamak, haritayı süresiz büyüten bir sızıntı olurdu.
	for jti, exp := range s.used {
		if !exp.After(now) {
			delete(s.used, jti)
		}
	}

	if _, seen := s.used[id]; seen {
		return ErrTicketUsed
	}

	s.used[id] = expiresAt

	return nil
}
