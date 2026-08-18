package orgs

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

func TestHasAtLeast(t *testing.T) {
	cases := []struct {
		have     store.OrgRole
		required store.OrgRole
		want     bool
	}{
		{store.OrgRoleOwner, store.OrgRoleOwner, true},
		{store.OrgRoleOwner, store.OrgRoleAdmin, true},
		{store.OrgRoleOwner, store.OrgRoleMember, true},
		{store.OrgRoleAdmin, store.OrgRoleOwner, false},
		{store.OrgRoleAdmin, store.OrgRoleAdmin, true},
		{store.OrgRoleAdmin, store.OrgRoleMember, true},
		{store.OrgRoleMember, store.OrgRoleOwner, false},
		{store.OrgRoleMember, store.OrgRoleAdmin, false},
		{store.OrgRoleMember, store.OrgRoleMember, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.have)+"→"+string(tc.required), func(t *testing.T) {
			if got := (Membership{Role: tc.have}).HasAtLeast(tc.required); got != tc.want {
				t.Errorf("HasAtLeast = %v, beklenen %v", got, tc.want)
			}
		})
	}
}

// Tanınmayan rol her zaman yetersiz sayılmalı: yetkilendirmede
// "bilmiyorum" ile "izin ver" aynı şey olamaz. (Enum'a yeni bir değer
// eklenip sıralamaya yazılmadığında bu test devreye giriyor.)
func TestHasAtLeastRejectsUnknownRoles(t *testing.T) {
	if (Membership{Role: store.OrgRole("superadmin")}).HasAtLeast(store.OrgRoleMember) {
		t.Error("bilinmeyen rol yeterli sayıldı")
	}
	if (Membership{Role: store.OrgRoleOwner}).HasAtLeast(store.OrgRole("superadmin")) {
		t.Error("bilinmeyen gereksinim karşılandı sayıldı")
	}
	if (Membership{}).HasAtLeast(store.OrgRoleMember) {
		t.Error("boş üyelik yeterli sayıldı")
	}
}

func TestMembershipContextRoundTrip(t *testing.T) {
	want := Membership{OrgID: uuid.New(), UserID: uuid.New(), Role: store.OrgRoleAdmin}

	got, ok := MembershipFrom(WithMembership(context.Background(), want))
	if !ok {
		t.Fatal("üyelik context'ten okunamadı")
	}
	if got != want {
		t.Errorf("üyelik = %+v, beklenen %+v", got, want)
	}
}

func TestMembershipFromEmptyContext(t *testing.T) {
	if _, ok := MembershipFrom(context.Background()); ok {
		t.Error("boş context üyelik döndürdü")
	}
}
