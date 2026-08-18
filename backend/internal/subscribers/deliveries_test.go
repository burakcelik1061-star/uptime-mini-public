package subscribers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// stubGate, 5.1'in görünürlük kapısının testlerdeki karşılığı.
//
// GERÇEK KAPI internal/statuspage'de ve KENDİ testleri var (6.2'de yazıldı);
// burada ölçülen şey, gönderici kapının cevabına gerçekten uyuyor mu.
type stubGate struct {
	visible bool
	// asked, kapıya sorulan monitor listelerini kaydeder.
	asked [][]uuid.UUID
}

func (g *stubGate) PublicSlugFor(
	_ context.Context, _ uuid.UUID, monitorIDs []uuid.UUID,
) (string, bool, error) {
	g.asked = append(g.asked, monitorIDs)

	if !g.visible {
		return "", false, nil
	}

	return "acme", true, nil
}

// confirmedSubscriber, onaylanmış bir abone yazar.
func (f *fixture) confirmedSubscriber(t *testing.T, email string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := f.pool.QueryRow(t.Context(), `
		INSERT INTO status_page_subscribers
			(status_page_id, email, status, confirmed_at, unsubscribe_token)
		VALUES ($1, $2, 'confirmed', now(), $3)
		RETURNING id`,
		f.pageID, email, "iptal-"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("abone yazılamadı: %v", err)
	}

	return id
}

// incident, kesinti ve kapsadığı servisi yazar.
//
// Zaman VERİTABANINDAN (`now() + offset`): 3.2'nin kararı — konteyner ile host
// saatleri arasındaki fark testi rastgele kılmasın.
func (f *fixture) incident(t *testing.T, title string, offset time.Duration, resolved bool) uuid.UUID {
	t.Helper()

	monitorID := f.monitor(t)

	status := "investigating"
	if resolved {
		status = "resolved"
	}

	var resolvedAt any
	if resolved {
		resolvedAt = fmt.Sprintf("%d microseconds", offset.Microseconds())
	}

	var id uuid.UUID
	err := f.pool.QueryRow(t.Context(), `
		INSERT INTO incidents (org_id, title, status, impact, started_at, resolved_at, is_auto_created)
		VALUES ($1, $2, $3, 'major', now() + $4::interval,
		        CASE WHEN $5::text IS NULL THEN NULL ELSE now() + $5::interval END, false)
		RETURNING id`,
		f.orgID, title, status, fmt.Sprintf("%d microseconds", offset.Microseconds()), resolvedAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("incident yazılamadı: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO incident_monitors (incident_id, monitor_id) VALUES ($1, $2)`,
		id, monitorID); err != nil {
		t.Fatalf("incident-monitor bağı yazılamadı: %v", err)
	}

	return id
}

func (f *fixture) monitor(t *testing.T) uuid.UUID {
	t.Helper()

	monitor, err := f.queries.CreateMonitor(t.Context(), store.CreateMonitorParams{
		OrgID:                f.orgID,
		Name:                 "Servis " + strings.ReplaceAll(uuid.NewString()[:8], "-", ""),
		Url:                  "https://ornek.example/health",
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: 60,
		TimeoutMs:            5000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		FailureThreshold:     3,
		RecoveryThreshold:    2,
		CurrentStatus:        store.MonitorStatusUp,
	})
	if err != nil {
		t.Fatalf("monitor oluşturulamadı: %v", err)
	}

	return monitor.ID
}

func (f *fixture) dispatcher(gate PublicGate) *Dispatcher {
	return NewDispatcher(f.queries, gate, f.mailer, "http://localhost:3100", discardLogger())
}

func (f *fixture) deliveryCount(t *testing.T) int {
	t.Helper()

	var total int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM subscriber_deliveries`).Scan(&total); err != nil {
		t.Fatalf("defter okunamadı: %v", err)
	}

	return total
}

// Onaylı abone kesinti açılışında bildirim alıyor ve bildirimde ÇALIŞAN bir
// iptal linki var.
func TestSubscriberIsNotifiedOnIncident(t *testing.T) {
	f := newFixture(t)

	f.confirmedSubscriber(t, "abone@example.com")
	f.incident(t, "Ödeme API yanıt vermiyor", 0, false)

	gate := &stubGate{visible: true}

	stats, err := f.dispatcher(gate).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 1 || stats.Sent != 1 {
		t.Fatalf("stats = %+v, beklenen 1 kuyruk / 1 gönderim", stats)
	}

	if len(f.mailer.targets) != 1 || f.mailer.targets[0] != "abone@example.com" {
		t.Fatalf("gönderilen e-postalar = %v", f.mailer.targets)
	}

	if !strings.Contains(f.mailer.bodies[0], "/unsubscribe?token=") {
		t.Errorf("bildirimde iptal linki yok: %s", f.mailer.bodies[0])
	}
}

// 6.4'ÜN SIZINTI TESTİ: kapı "görünmez" dediğinde aboneye HİÇBİR ŞEY
// gitmiyor. Abonenin gördüğü, sayfada göreceğinden fazlası olamaz.
func TestHiddenIncidentIsNotSentToSubscribers(t *testing.T) {
	f := newFixture(t)

	f.confirmedSubscriber(t, "abone@example.com")
	f.incident(t, "Ic API yanıt vermiyor", 0, false)

	gate := &stubGate{visible: false}

	stats, err := f.dispatcher(gate).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Hidden != 1 || stats.Enqueued != 0 || stats.Sent != 0 {
		t.Fatalf("stats = %+v, beklenen 1 gizli / 0 gönderim", stats)
	}

	if len(f.mailer.targets) != 0 {
		t.Fatalf("gizli kesinti aboneye gönderildi: %v", f.mailer.targets)
	}

	if got := f.deliveryCount(t); got != 0 {
		t.Fatalf("defter satırı = %d, beklenen 0", got)
	}

	// KAPIYA KESİNTİNİN SERVİSLERİ SORULMUŞ olmalı: karar başka bir yerde
	// yeniden hesaplanmıyor.
	if len(gate.asked) != 1 || len(gate.asked[0]) != 1 {
		t.Errorf("kapıya sorulan liste = %v", gate.asked)
	}
}

// 6.3'ün garantisinin aynısı: aynı aboneye aynı olay iki kez gitmiyor.
func TestSubscriberDeliveryIsNotSentTwice(t *testing.T) {
	f := newFixture(t)

	f.confirmedSubscriber(t, "abone@example.com")
	f.incident(t, "Kesinti", 0, false)

	dispatcher := f.dispatcher(&stubGate{visible: true})

	for range 3 {
		if _, err := dispatcher.RunOnce(t.Context()); err != nil {
			t.Fatalf("beklenmeyen hata: %v", err)
		}
	}

	if len(f.mailer.targets) != 1 {
		t.Fatalf("gönderim sayısı = %d, beklenen 1 (üç tur koştu)", len(f.mailer.targets))
	}

	if got := f.deliveryCount(t); got != 1 {
		t.Fatalf("defter satırı = %d, beklenen 1", got)
	}
}

// ONAYLANMAMIŞ ABONE BİLDİRİM ALMIYOR: çift onayın anlamı bu.
func TestPendingSubscriberIsNotNotified(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), f.slug, "bekleyen@example.com"); err != nil {
		t.Fatalf("abonelik: %v", err)
	}

	f.incident(t, "Kesinti", 0, false)

	// Onay e-postası zaten gönderildi; bildirim sayacını ondan ayırmak için
	// mailer sıfırlanıyor.
	f.mailer.targets = nil
	f.mailer.bodies = nil

	stats, err := f.dispatcher(&stubGate{visible: true}).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 0 || len(f.mailer.targets) != 0 {
		t.Fatalf("onaylanmamış aboneye bildirim gitti: %+v / %v", stats, f.mailer.targets)
	}
}

// YENİ ABONE GEÇMİŞİ POSTA KUTUSUNDA BULMUYOR: olay abonelikten ÖNCEYSE
// kuyruğa girmiyor (6.3'ün kararının aynısı).
func TestSubscriberDoesNotReceiveOlderIncidents(t *testing.T) {
	f := newFixture(t)

	// Kesinti önce, abone sonra.
	f.incident(t, "Aboneden önceki kesinti", -time.Hour, false)
	f.confirmedSubscriber(t, "abone@example.com")

	stats, err := f.dispatcher(&stubGate{visible: true}).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 0 || len(f.mailer.targets) != 0 {
		t.Fatalf("geçmiş kesinti bildirildi: %+v / %v", stats, f.mailer.targets)
	}
}

// Kapanış AYRI bir olay.
func TestSubscriberIsNotifiedOnResolve(t *testing.T) {
	f := newFixture(t)

	f.confirmedSubscriber(t, "abone@example.com")
	incidentID := f.incident(t, "Kesinti", 0, false)

	dispatcher := f.dispatcher(&stubGate{visible: true})

	if _, err := dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatalf("1. tur: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE incidents SET resolved_at = now(), status = 'resolved' WHERE id = $1`,
		incidentID); err != nil {
		t.Fatalf("incident kapatılamadı: %v", err)
	}

	if _, err := dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatalf("2. tur: %v", err)
	}

	if len(f.mailer.targets) != 2 {
		t.Fatalf("gönderim sayısı = %d, beklenen 2", len(f.mailer.targets))
	}

	if got := f.deliveryCount(t); got != 2 {
		t.Fatalf("defter satırı = %d, beklenen 2", got)
	}
}
