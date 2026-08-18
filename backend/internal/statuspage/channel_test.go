package statuspage

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// 6.2'nin SIZINTI TESTLERİ. Kural 5.1'de kondu: yayımlanmamış bir servis
// public yüzeyin hiçbir alanından çıkmaz. Canlı yayında bu, "o servisin durum
// değişikliği public kanala HİÇ ÇIKMAZ" demek — gövdesiz bir sinyal bile
// çıkmamalı, çünkü sinyalin kendisi "bu org'da şu anda private bir servis
// düştü" bilgisini taşır.

func TestPublicSlugForPublishedMonitor(t *testing.T) {
	f := newFixture(t)

	monitor := f.monitor(t, "Public API", store.MonitorStatusUp)
	f.publish(t, monitor.ID)

	slug, ok, err := f.svc.PublicSlugFor(t.Context(), f.orgID, []uuid.UUID{monitor.ID})
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if !ok {
		t.Fatal("yayımlanmış servis için public kanal dönmedi")
	}

	if slug != f.page.Slug {
		t.Errorf("slug = %q, beklenen %q", slug, f.page.Slug)
	}
}

// ASIL TEST: private bir servisin değişikliği public kanala çıkmıyor.
func TestPublicSlugForPrivateMonitorIsSilent(t *testing.T) {
	f := newFixture(t)

	published := f.monitor(t, "Public API", store.MonitorStatusUp)
	private := f.monitor(t, "Ic API", store.MonitorStatusUp)

	f.publish(t, published.ID)

	_, ok, err := f.svc.PublicSlugFor(t.Context(), f.orgID, []uuid.UUID{private.ID})
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if ok {
		t.Fatal("yayımlanmamış servisin değişikliği public kanala çıktı")
	}
}

// Manuel incident hem public hem private servisi kapsıyorsa yayın YAPILIYOR:
// 5.1'in kuralı da böyle (incident görünür, ama etkilenen servis listesinde
// yalnızca public olanlar dönüyor). Kapı incident'ın GÖRÜNÜRLÜĞÜNÜ ölçüyor,
// içeriğini değil — içeriği zaten public sorgular süzüyor.
func TestPublicSlugForMixedMonitorsBroadcasts(t *testing.T) {
	f := newFixture(t)

	published := f.monitor(t, "Public API", store.MonitorStatusUp)
	private := f.monitor(t, "Ic API", store.MonitorStatusUp)

	f.publish(t, published.ID)

	_, ok, err := f.svc.PublicSlugFor(t.Context(), f.orgID, []uuid.UUID{private.ID, published.ID})
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if !ok {
		t.Fatal("public servisi de kapsayan değişiklik yayımlanmadı")
	}
}

// YAYIMLANMAMIŞ SAYFA HİÇ YAYIN YAPMIYOR: okuma ucunda 404 alan bir ziyaretçi,
// canlı kanaldan değişiklik duyamamalı.
func TestPublicSlugForUnpublishedPageIsSilent(t *testing.T) {
	f := newFixture(t)

	monitor := f.monitor(t, "Public API", store.MonitorStatusUp)
	// Servis SEÇİLİ ama sayfa YAYIMLANMAMIŞ: `publish` yardımcısı ikisini
	// birlikte yapıyor, burada bilinçli olarak yalnızca seçim yazılıyor.
	if _, err := f.svc.ReplaceServices(t.Context(), f.orgID, ungrouped(monitor.ID)); err != nil {
		t.Fatalf("servisler seçilemedi: %v", err)
	}

	_, ok, err := f.svc.PublicSlugFor(t.Context(), f.orgID, []uuid.UUID{monitor.ID})
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if ok {
		t.Fatal("yayımlanmamış sayfa için public kanal döndü")
	}
}

func TestPublicSlugForUnknownOrgIsSilent(t *testing.T) {
	f := newFixture(t)

	_, ok, err := f.svc.PublicSlugFor(t.Context(), uuid.New(), []uuid.UUID{uuid.New()})
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if ok {
		t.Fatal("olmayan organizasyon için public kanal döndü")
	}
}

func TestPublicSlugForEmptyMonitorListIsSilent(t *testing.T) {
	f := newFixture(t)

	_, ok, err := f.svc.PublicSlugFor(t.Context(), f.orgID, nil)
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if ok {
		t.Fatal("boş servis listesi için public kanal döndü")
	}
}

// PublishedSlug, public WebSocket'in kapısı: okuma ucuyla AYNI kararı vermeli.
func TestPublishedSlugMatchesReadEndpoint(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.PublishedSlug(t.Context(), f.page.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("yayımlanmamış sayfa için hata = %v, beklenen ErrNotFound", err)
	}

	f.publish(t)

	if err := f.svc.PublishedSlug(t.Context(), f.page.Slug); err != nil {
		t.Fatalf("yayımlanmış sayfa reddedildi: %v", err)
	}

	if err := f.svc.PublishedSlug(t.Context(), "hic-olmayan-slug"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("olmayan slug için hata = %v, beklenen ErrNotFound", err)
	}
}
