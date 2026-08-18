package orgs

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Membership, bir kullanıcının belirli bir organizasyondaki erişimi.
//
// Org-scoped bir istek bu değeri TEK BİR kez (middleware'de, veritabanından)
// üretiyor; sonraki handler'lar tekrar sorgu yapmıyor.
type Membership struct {
	OrgID   uuid.UUID
	UserID  uuid.UUID
	Role    store.OrgRole
	OrgName string
	OrgSlug string
	// OrgPlan, organizasyonun planı (8.5).
	//
	// Üyelik sorgusunda ZATEN okunuyor, yani fazladan bir gidiş-geliş yok.
	// Kotayı UYGULAYAN yer burası DEĞİL (o `internal/monitors`, kilitli
	// satırdan okuyarak); bu alan arayüzün "hangi plandayım" sorusunu
	// cevaplaması için.
	OrgPlan  store.OrgPlan
	OrgSince time.Time
}

// roleRank, rollerin yetki sıralaması. Rol gereksinimleri "en az şu rol"
// biçiminde ifade ediliyor: owner her yerde admin'in, admin de member'ın
// yapabildiğini yapabilir.
//
// Sıralama yerine "izin verilen roller kümesi" de kullanılabilirdi ama o
// zaman her yeni uçta owner'ı listeye eklemeyi unutmak mümkün olurdu —
// sessizce yetkisiz kalan bir kurucu, fark edilmesi zor bir hata.
var roleRank = map[store.OrgRole]int{
	store.OrgRoleMember: 1,
	store.OrgRoleAdmin:  2,
	store.OrgRoleOwner:  3,
}

// HasAtLeast, üyeliğin verilen rolün gerektirdiği yetkiye sahip olup
// olmadığını söyler.
//
// Bilinmeyen bir rol (ör. enum'a sonradan eklenip burada karşılığı
// unutulmuş bir değer) her zaman YETERSİZ sayılıyor: yetkilendirmede
// "tanımıyorum" ile "izin ver" aynı şey olamaz.
func (m Membership) HasAtLeast(required store.OrgRole) bool {
	have, ok := roleRank[m.Role]
	if !ok {
		return false
	}

	need, ok := roleRank[required]
	if !ok {
		return false
	}

	return have >= need
}

// membershipContextKey, context anahtarının tipi (bkz. auth.Identity —
// aynı gerekçe: dışa kapalı tip, çakışmayı derleyici seviyesinde önlüyor).
type membershipContextKey struct{}

// WithMembership, üyeliği context'e yazar.
func WithMembership(ctx context.Context, membership Membership) context.Context {
	return context.WithValue(ctx, membershipContextKey{}, membership)
}

// MembershipFrom, context'teki üyeliği döner. İkinci dönüş değeri, isteğin
// org-scoped middleware'den geçip geçmediğini söyler.
func MembershipFrom(ctx context.Context) (Membership, bool) {
	membership, ok := ctx.Value(membershipContextKey{}).(Membership)

	return membership, ok
}
