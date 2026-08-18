package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// writeIncidents, kesinti geçmişini üretir.
//
// Üç kapanmış + bir AÇIK kesinti: status page'in "geçmiş" ve "şu an devam
// eden" iki yüzü de dolu görünsün. Biri MANUEL (birden fazla servisi
// kapsıyor — 0.1'in çoktan-çoğa modelinin görünür hâli), biri
// YAYIMLANMAMIŞ servise ait (5.1'in sızıntı kuralı demo veride de sınansın:
// o kesinti public sayfada YOK).
func (b *builder) writeIncidents(
	ctx context.Context,
	orgID, userID uuid.UUID,
	monitors []store.Monitor,
	specs []demoMonitor,
) (int, error) {
	now := b.now().UTC()

	type update struct {
		after  time.Duration
		status store.IncidentStatus
		body   string
		system bool
	}

	type incidentSpec struct {
		title    string
		impact   store.IncidentImpact
		auto     bool
		monitor  int // monitors dizisindeki indeks; -1 = manuel (çoklu)
		covers   []int
		started  time.Duration // bugünden geriye
		duration time.Duration // 0 = hâlâ açık
		updates  []update
	}

	specsList := []incidentSpec{
		{
			// AÇIK kesinti — panodaki ve status page'deki "şu an" kaydı.
			title: "Ödeme Servisi yanıt vermiyor", impact: store.IncidentImpactMajor,
			auto: true, monitor: 2, covers: []int{2},
			started: 35 * time.Minute,
			updates: []update{
				{0, store.IncidentStatusInvestigating,
					"Otomatik olarak açıldı: ardışık 3 başarısız kontrol.", true},
				{12 * time.Minute, store.IncidentStatusIdentified,
					"Ödeme sağlayıcısının uç noktası 503 döndürüyor. Sağlayıcıyla iletişimdeyiz.", false},
			},
		},
		{
			title: "API gecikmeleri ve kesinti", impact: store.IncidentImpactMajor,
			auto: true, monitor: 1, covers: []int{1},
			started: 12*24*time.Hour + 3*time.Hour, duration: 2*time.Hour + 40*time.Minute,
			updates: []update{
				{0, store.IncidentStatusInvestigating,
					"Otomatik olarak açıldı: ardışık 3 başarısız kontrol.", true},
				{25 * time.Minute, store.IncidentStatusIdentified,
					"Veritabanı bağlantı havuzu doldu; kaynak, yeni sürümdeki bir sorgu.", false},
				{2*time.Hour + 40*time.Minute, store.IncidentStatusResolved,
					"Sorgu geri alındı, tüm ölçümler normale döndü.", false},
			},
		},
		{
			// MANUEL ve İKİ SERVİSİ kapsıyor.
			title: "Planlı olmayan veri merkezi bakımı", impact: store.IncidentImpactMinor,
			auto: false, monitor: -1, covers: []int{0, 3},
			started: 41*24*time.Hour + 8*time.Hour, duration: 55 * time.Minute,
			updates: []update{
				{0, store.IncidentStatusInvestigating,
					"Sağlayıcımız acil bir ağ bakımı ilan etti; kısa kesintiler bekleniyor.", false},
				{55 * time.Minute, store.IncidentStatusResolved,
					"Bakım tamamlandı, servisler normale döndü.", false},
			},
		},
		{
			// YAYIMLANMAMIŞ servisin kesintisi: public sayfada GÖRÜNMEMELİ.
			// Başlığında monitor adı geçiyor — 4.1'in başlık tuzağı demo
			// veride de var, yani sızıntı kuralı gerçekten sınanıyor.
			title: "Iç Yedekleme Işi başarısız", impact: store.IncidentImpactMinor,
			auto: true, monitor: 4, covers: []int{4},
			started: 3*24*time.Hour + 2*time.Hour, duration: 90 * time.Minute,
			updates: []update{
				{0, store.IncidentStatusInvestigating,
					"Otomatik olarak açıldı: ardışık 3 başarısız kontrol.", true},
				{90 * time.Minute, store.IncidentStatusResolved,
					"Otomatik olarak çözüldü: servis yeniden yanıt veriyor.", true},
			},
		},
	}

	for _, spec := range specsList {
		startedAt := now.Add(-spec.started)

		var resolvedAt any
		status := store.IncidentStatusInvestigating
		if spec.duration > 0 {
			resolvedAt = startedAt.Add(spec.duration)
			status = store.IncidentStatusResolved
		} else if len(spec.updates) > 0 {
			status = spec.updates[len(spec.updates)-1].status
		}

		var autoMonitor any
		if spec.auto {
			autoMonitor = monitors[spec.monitor].ID
		}

		var incidentID uuid.UUID
		if err := b.tx.QueryRow(ctx, `
			INSERT INTO incidents (
				org_id, title, status, impact, auto_monitor_id, is_auto_created,
				started_at, resolved_at, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7::timestamptz, $8::timestamptz,
			        $7::timestamptz, coalesce($8::timestamptz, $7::timestamptz))
			RETURNING id`,
			orgID, spec.title, status, spec.impact, autoMonitor, spec.auto,
			startedAt, resolvedAt).Scan(&incidentID); err != nil {
			return 0, fmt.Errorf("kesinti yazılamadı (%s): %w", spec.title, err)
		}

		for _, index := range spec.covers {
			if _, err := b.tx.Exec(ctx,
				`INSERT INTO incident_monitors (incident_id, monitor_id) VALUES ($1, $2)`,
				incidentID, monitors[index].ID); err != nil {
				return 0, fmt.Errorf("kesinti servisi bağlanamadı: %w", err)
			}
		}

		for _, u := range spec.updates {
			var author any
			if !u.system {
				author = userID
			}

			if _, err := b.tx.Exec(ctx, `
				INSERT INTO incident_updates (incident_id, author_id, is_system, status, body, created_at)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				incidentID, author, u.system, u.status, u.body,
				startedAt.Add(u.after)); err != nil {
				return 0, fmt.Errorf("timeline satırı yazılamadı: %w", err)
			}
		}
	}

	return len(specsList), nil
}

// writeMaintenance, YAKLAŞAN bir bakım penceresi yazar.
//
// Geçmişe tarihli pencere YAZILMIYOR: 4.3 `ends_at`in gelecekte olmasını şart
// koşuyor ve geçmiş bir pencere `maintenance_checks` ile tutarsız kalırdı
// (o sütunu doldurmak, 90 günlük özetleri de yeniden hesaplamak demekti).
func (b *builder) writeMaintenance(ctx context.Context, orgID, monitorID uuid.UUID) error {
	startsAt := b.now().UTC().Add(48 * time.Hour)

	var windowID uuid.UUID
	if err := b.tx.QueryRow(ctx, `
		INSERT INTO maintenance_windows (org_id, title, description, starts_at, ends_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		orgID,
		"CDN sağlayıcı bakımı",
		"CDN sağlayıcımız bölgesel bir bakım ilan etti; kısa süreli önbellek boşalmaları olabilir.",
		startsAt, startsAt.Add(2*time.Hour)).Scan(&windowID); err != nil {
		return fmt.Errorf("bakım penceresi yazılamadı: %w", err)
	}

	if _, err := b.tx.Exec(ctx,
		`INSERT INTO maintenance_window_monitors (window_id, monitor_id) VALUES ($1, $2)`,
		windowID, monitorID); err != nil {
		return fmt.Errorf("bakım penceresine servis bağlanamadı: %w", err)
	}

	return nil
}

// publishStatusPage, servisleri gruplarıyla yayımlar ve sayfayı YAYINA AÇAR.
//
// Yayımlanmamış bir sayfa ziyaretçiye 404 veriyor (5.1), yani yayına
// açılmadan demo'nun en gösterişli ekranı görünmezdi.
func (b *builder) publishStatusPage(
	ctx context.Context, orgID uuid.UUID, monitors []store.Monitor, specs []demoMonitor,
) error {
	var pageID uuid.UUID
	if err := b.tx.QueryRow(ctx,
		`SELECT id FROM status_pages WHERE org_id = $1`, orgID).Scan(&pageID); err != nil {
		return fmt.Errorf("status page bulunamadı: %w", err)
	}

	if _, err := b.tx.Exec(ctx, `
		UPDATE status_pages
		SET is_published = true, title = $2, description = $3, theme_color = $4
		WHERE id = $1`,
		pageID,
		"Acme Corp Durum",
		"Acme Corp servislerinin güncel durumu ve kesinti geçmişi.",
		"#5b9dff"); err != nil {
		return fmt.Errorf("status page yayımlanamadı: %w", err)
	}

	// Gruplar: sıra kullanıcının (5.3), yani `position` ile.
	groups := map[string]uuid.UUID{}
	for position, name := range []string{"Uygulama", "Altyapı"} {
		var groupID uuid.UUID
		if err := b.tx.QueryRow(ctx, `
			INSERT INTO status_page_groups (status_page_id, name, position)
			VALUES ($1, $2, $3)
			RETURNING id`, pageID, name, position).Scan(&groupID); err != nil {
			return fmt.Errorf("grup yazılamadı (%s): %w", name, err)
		}
		groups[name] = groupID
	}

	for i, spec := range specs {
		if !spec.public {
			continue
		}

		var groupID any
		if id, ok := groups[spec.group]; ok {
			groupID = id
		}

		if _, err := b.tx.Exec(ctx, `
			INSERT INTO status_page_items (status_page_id, monitor_id, group_id)
			VALUES ($1, $2, $3)`, pageID, monitors[i].ID, groupID); err != nil {
			return fmt.Errorf("yayımlanan servis yazılamadı (%s): %w", spec.name, err)
		}
	}

	return nil
}

// writeNotificationChannel, bir webhook kanalı ekler (6.3'ün ekranı boş
// kalmasın).
//
// E-POSTA KANALI DEĞİL: e-posta adapter'ı hâlâ mock (GÜNCEL DURUM §2),
// webhook ise 7.4'te gerçek gönderici oldu. Hedef `example.com`, yani
// gönderim denendiğinde SSRF koruması devrede ve adres genel — demo bir iç
// ağ adresi göstererek korumayı yanlış tanıtmıyor.
func (b *builder) writeNotificationChannel(ctx context.Context, orgID uuid.UUID) error {
	if _, err := b.tx.Exec(ctx, `
		INSERT INTO notification_channels (org_id, kind, name, target)
		VALUES ($1, $2, $3, $4)`,
		orgID, store.NotificationChannelKindWebhook,
		"Nöbetçi kanalı", "https://example.com/hooks/uptime"); err != nil {
		return fmt.Errorf("bildirim kanalı yazılamadı: %w", err)
	}

	return nil
}
