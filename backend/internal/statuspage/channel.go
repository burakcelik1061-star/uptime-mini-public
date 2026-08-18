package statuspage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Canlı yayının PUBLIC tarafının kapısı (Faz 6.2).
//
// 5.1'in sızıntı kuralı canlı yayında da geçerli: bir org'un public kanalına
// yalnızca YAYIMLANMIŞ servisleri ilgilendiren bir değişiklik girebilir.
// Yayımlanmamış bir monitor'ün durum değişikliği kanala hiç çıkmıyor — çünkü
// "bir şey değişti" sinyali bile, o organizasyonda private bir servisin o anda
// düştüğünü söyleyen bir zamanlama bilgisidir.
//
// KURAL BURADA YENİDEN YAZILMIYOR. Bu fonksiyon yeni bir SQL yüklemi
// tanımlamıyor; 5.1'in KENDİ sorgularını çağırıyor (`GetStatusPageForOrg` +
// `ListPublicServices`). Sebep doğrudan: "yayımlanan servis" tanımının ikinci
// bir kopyası olsaydı, sızıntı tam olarak iki kopyanın ayrıştığı gün doğardı
// (paket dokümanının ilk paragrafı bu yüzden yazılmıştı).
//
// 4.1'in "otomatik incident BAŞLIĞI monitor adını içeriyor" tuzağı da bununla
// kapanıyor: otomatik bir incident'ın öznesi tek bir monitor ve kapı tam olarak
// o monitor'e bakıyor, yani özne yayımlanmamışsa kanala hiçbir şey çıkmıyor.

// PublicSlugFor, verilen monitor'lerden EN AZ BİRİ yayımlanmışsa
// organizasyonun public sayfa slug'ını döner.
//
// İkinci dönüş değeri false ise public kanal YOK: sayfa yayımlanmamış olabilir
// ya da değişiklik yalnızca private servisleri ilgilendiriyordur. İkisi de
// aynı sonucu doğuruyor (yayın yok) — 5.1'in "yayımlanmamış ≡ olmayan"
// disiplini burada da geçerli.
func (s *Service) PublicSlugFor(
	ctx context.Context, orgID uuid.UUID, monitorIDs []uuid.UUID,
) (string, bool, error) {
	if len(monitorIDs) == 0 {
		return "", false, nil
	}

	page, err := s.queries.GetStatusPageForOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("status page okunamadı: %w", err)
	}

	// YAYIMLANMAMIŞ SAYFA HİÇ YAYIN YAPMIYOR: public uç de onu 404'lüyor
	// (GetPublishedStatusPage), yani kanalı açık tutmak "sayfayı okuyamayan
	// ama değişikliklerini duyabilen ziyaretçi" demek olurdu.
	if !page.IsPublished {
		return "", false, nil
	}

	published, err := s.queries.ListPublicServices(ctx, page.ID)
	if err != nil {
		return "", false, fmt.Errorf("yayımlanan servisler okunamadı: %w", err)
	}

	publishedIDs := make(map[uuid.UUID]struct{}, len(published))
	for _, service := range published {
		publishedIDs[service.MonitorID] = struct{}{}
	}

	for _, monitorID := range monitorIDs {
		if _, ok := publishedIDs[monitorID]; ok {
			return page.Slug, true, nil
		}
	}

	return "", false, nil
}

// PublishedSlug, slug'ı YAYIMLANMIŞSA doğrular (public WebSocket'in kapısı).
//
// Ayrı bir sorgu yok: public okuma ucunun kullandığı `GetPublishedStatusPage`
// çağrılıyor, yani "yayımlanmamış ≡ olmayan" kararı tek bir yerde duruyor.
func (s *Service) PublishedSlug(ctx context.Context, slug string) error {
	if _, err := s.queries.GetPublishedStatusPage(ctx, slug); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}

		return fmt.Errorf("status page okunamadı: %w", err)
	}

	return nil
}
