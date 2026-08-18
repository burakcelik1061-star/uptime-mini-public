package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestIdentityRoundTrip(t *testing.T) {
	want := Identity{UserID: uuid.New(), TokenID: uuid.NewString()}

	got, ok := IdentityFrom(WithIdentity(context.Background(), want))
	if !ok {
		t.Fatal("kimlik context'ten okunamadı")
	}
	if got != want {
		t.Errorf("kimlik = %+v, beklenen %+v", got, want)
	}
}

func TestIdentityFromEmptyContext(t *testing.T) {
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Error("boş context kimlik döndürdü")
	}
}

// Anahtarın tipi dışa kapalı olduğu için, başka bir paketin aynı ADI
// kullanması kimliği ne ezebilir ne de taklit edebilir. Ham string anahtar
// kullanılsaydı bu test kimliği okuyabilirdi.
func TestRawStringKeyCannotImpersonateIdentity(t *testing.T) {
	type otherPackageKey string

	ctx := context.WithValue(context.Background(), otherPackageKey("identityContextKey"), Identity{UserID: uuid.New()})
	//nolint:staticcheck // ham string anahtarın çakışmadığını göstermek testin konusu.
	ctx = context.WithValue(ctx, "identityContextKey", Identity{UserID: uuid.New()})

	if _, ok := IdentityFrom(ctx); ok {
		t.Error("dışarıdan yazılan değer kimlik olarak okundu")
	}
}
