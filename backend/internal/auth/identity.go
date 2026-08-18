package auth

import (
	"context"

	"github.com/google/uuid"
)

// Identity, doğrulanmış token'dan çıkan kimlik.
//
// Kullanıcının adı/email'i BİLİNÇLİ olarak yok: token'a konan her alan,
// kullanıcı verisi değişse bile token'ın ömrü boyunca eski değeriyle
// dolaşır. Kimliğin tek doğruluk kaynağı veritabanı; token yalnızca
// "bu istek kimin adına" sorusunu cevaplıyor.
type Identity struct {
	// UserID, token'ın `sub` alanı.
	UserID uuid.UUID
	// TokenID, token'ın `jti` alanı — loglama ve Faz 1.2'nin iptal akışı için.
	TokenID string
}

// identityContextKey, context anahtarının tipi.
//
// Ham string ("user") ile yazılsaydı, aynı context'i paylaşan BAŞKA bir
// paketin aynı string'i kullanması değeri sessizce ezerdi; dışa açık bir
// string ise üçüncü taraf kodun kimliği taklit etmesine imkân verirdi.
// Dışa kapalı bir tip, çakışmayı derleyici seviyesinde imkânsız kılıyor.
type identityContextKey struct{}

// WithIdentity, kimliği context'e yazar.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFrom, context'teki kimliği döner. İkinci dönüş değeri, isteğin
// kimlik doğrulamasından geçip geçmediğini söyler.
//
// Panikleyen bir "MustIdentity" bilinçli olarak YOK: kimliğin var olduğu
// varsayımı, middleware'i takmayı unutulan bir rotada süreci düşürürdü.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)

	return identity, ok
}
