package notifications

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingChannel, gönderilen mesajları kaydeden test adapter'ı.
//
// `Channel` arayüzünün ikinci bir uygulaması olması 6.3'ün soyutlamasının
// gerçekten işe yaradığının kanıtı: gönderici, adapter'ın hangisi olduğunu
// bilmiyor.
type recordingChannel struct {
	kind     store.NotificationChannelKind
	sent     []Message
	targets  []string
	failWith error
}

func (c *recordingChannel) Kind() store.NotificationChannelKind { return c.kind }

func (c *recordingChannel) Send(_ context.Context, target string, msg Message) error {
	if c.failWith != nil {
		return c.failWith
	}

	c.sent = append(c.sent, msg)
	c.targets = append(c.targets, target)

	return nil
}

type fixture struct {
	pool    *pgxpool.Pool
	queries *store.Queries
	orgID   uuid.UUID
	orgName string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	pool := testdb.Pool(t, "notifications")
	queries := store.New(pool)

	slug := "org-" + strings.ReplaceAll(uuid.NewString(), "-", "")

	org, err := queries.CreateOrganization(t.Context(), store.CreateOrganizationParams{
		Name: "Acme A.Ş.",
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("organizasyon oluşturulamadı: %v", err)
	}

	return &fixture{pool: pool, queries: queries, orgID: org.ID, orgName: org.Name}
}

func (f *fixture) channel(t *testing.T, kind store.NotificationChannelKind, target string) store.NotificationChannel {
	t.Helper()

	channel, err := f.queries.CreateNotificationChannel(t.Context(), store.CreateNotificationChannelParams{
		OrgID:  f.orgID,
		Kind:   kind,
		Name:   "Nöbetçi",
		Target: target,
	})
	if err != nil {
		t.Fatalf("kanal oluşturulamadı: %v", err)
	}

	return channel
}

// incident, doğrudan SQL ile bir kesinti yazar (incident yazma yolunun 6.3'ten
// habersiz olduğunu vurgulamak için: burada hiçbir bildirim kodu çağrılmıyor).
//
// ZAMAN VERİTABANINDAN (`now() + offset`), Go sürecinden DEĞİL: 3.2'nin
// kararının aynısı. Testler konteynerdeki Postgres'e karşı koşuyor ve iki
// saat arasındaki milisaniyelik fark, "olay kanaldan sonra mı" karşılaştırmasını
// rastgele iki yönde de sonuçlandırabilirdi.
func (f *fixture) incident(t *testing.T, title string, startedOffset time.Duration, resolved bool) uuid.UUID {
	t.Helper()

	status := "investigating"
	if resolved {
		status = "resolved"
	}

	var resolvedAt any
	if resolved {
		resolvedAt = "0 seconds"
	}

	var id uuid.UUID
	err := f.pool.QueryRow(t.Context(), `
		INSERT INTO incidents (org_id, title, status, impact, started_at, resolved_at, is_auto_created)
		VALUES ($1, $2, $3, 'major', now() + $4::interval,
		        CASE WHEN $5::text IS NULL THEN NULL ELSE now() + $5::interval END, false)
		RETURNING id`,
		f.orgID, title, status, intervalText(startedOffset), resolvedAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("incident yazılamadı: %v", err)
	}

	return id
}

// intervalText, Go süresini Postgres interval metnine çevirir.
func intervalText(d time.Duration) string {
	return fmt.Sprintf("%d microseconds", d.Microseconds())
}

func (f *fixture) dispatcher(t *testing.T, adapters ...Channel) *Dispatcher {
	t.Helper()

	return NewDispatcher(f.queries, NewRegistryWith(adapters...), discardLogger(), DispatcherOptions{})
}

func (f *fixture) deliveries(t *testing.T) []struct {
	Event    store.NotificationEvent
	Status   store.NotificationStatus
	Attempts int32
} {
	t.Helper()

	rows, err := f.pool.Query(t.Context(),
		`SELECT event, status, attempts FROM notification_deliveries ORDER BY event`)
	if err != nil {
		t.Fatalf("defter okunamadı: %v", err)
	}
	defer rows.Close()

	var result []struct {
		Event    store.NotificationEvent
		Status   store.NotificationStatus
		Attempts int32
	}

	for rows.Next() {
		var row struct {
			Event    store.NotificationEvent
			Status   store.NotificationStatus
			Attempts int32
		}
		if err := rows.Scan(&row.Event, &row.Status, &row.Attempts); err != nil {
			t.Fatalf("satır okunamadı: %v", err)
		}

		result = append(result, row)
	}

	return result
}

// Açılan bir kesinti, incident yazma yoluna HİÇ DOKUNULMADAN bildiriliyor:
// job tarıyor, deftere yazıyor ve gönderiyor.
func TestDispatcherSendsOpenedIncident(t *testing.T) {
	f := newFixture(t)

	channel := f.channel(t, store.NotificationChannelKindEmail, "nobetci@example.com")
	f.incident(t, "Ödeme API yanıt vermiyor", 0, false)

	adapter := &recordingChannel{kind: store.NotificationChannelKindEmail}

	stats, err := f.dispatcher(t, adapter).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 1 || stats.Sent != 1 {
		t.Fatalf("stats = %+v, beklenen 1 kuyruk / 1 gönderim", stats)
	}

	if len(adapter.sent) != 1 {
		t.Fatalf("gönderilen mesaj sayısı = %d", len(adapter.sent))
	}

	if adapter.targets[0] != channel.Target {
		t.Errorf("hedef = %q, beklenen %q", adapter.targets[0], channel.Target)
	}

	msg := adapter.sent[0]
	if msg.Event != store.NotificationEventIncidentOpened {
		t.Errorf("olay = %q", msg.Event)
	}
	if msg.Title != "Ödeme API yanıt vermiyor" {
		t.Errorf("başlık = %q", msg.Title)
	}
	if msg.OrgName != f.orgName {
		t.Errorf("org adı = %q, beklenen %q", msg.OrgName, f.orgName)
	}
}

// 6.3'ÜN ASIL GARANTİSİ: aynı bildirim iki kez gönderilmiyor. Garanti
// uygulamada değil ŞEMADA — (channel, incident, event) UNIQUE kısıtı.
func TestDispatcherDoesNotSendTwice(t *testing.T) {
	f := newFixture(t)

	f.channel(t, store.NotificationChannelKindEmail, "nobetci@example.com")
	f.incident(t, "Kesinti", 0, false)

	adapter := &recordingChannel{kind: store.NotificationChannelKindEmail}
	dispatcher := f.dispatcher(t, adapter)

	for range 3 {
		if _, err := dispatcher.RunOnce(t.Context()); err != nil {
			t.Fatalf("beklenmeyen hata: %v", err)
		}
	}

	if len(adapter.sent) != 1 {
		t.Fatalf("gönderim sayısı = %d, beklenen 1 (üç tur koştu)", len(adapter.sent))
	}

	rows := f.deliveries(t)
	if len(rows) != 1 || rows[0].Status != store.NotificationStatusSent {
		t.Fatalf("defter = %+v", rows)
	}
}

// Kapanan kesinti AYRI bir olay: açılış zaten gönderilmiş olsa da kapanış
// gönderiliyor.
func TestDispatcherSendsResolvedAsSeparateEvent(t *testing.T) {
	f := newFixture(t)

	f.channel(t, store.NotificationChannelKindEmail, "nobetci@example.com")
	incidentID := f.incident(t, "Kesinti", 0, false)

	adapter := &recordingChannel{kind: store.NotificationChannelKindEmail}
	dispatcher := f.dispatcher(t, adapter)

	if _, err := dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE incidents SET resolved_at = now(), status = 'resolved' WHERE id = $1`,
		incidentID); err != nil {
		t.Fatalf("incident kapatılamadı: %v", err)
	}

	if _, err := dispatcher.RunOnce(t.Context()); err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if len(adapter.sent) != 2 {
		t.Fatalf("gönderim sayısı = %d, beklenen 2", len(adapter.sent))
	}

	if adapter.sent[1].Event != store.NotificationEventIncidentResolved {
		t.Errorf("ikinci olay = %q", adapter.sent[1].Event)
	}

	if adapter.sent[1].ResolvedAt == nil {
		t.Error("kapanış mesajında ResolvedAt yok")
	}
}

// YENİ KANAL GEÇMİŞİ POSTA KUTUSUNA BOŞALTMIYOR: olay kanaldan ÖNCE
// gerçekleştiyse kuyruğa hiç girmiyor.
func TestDispatcherIgnoresIncidentsOlderThanChannel(t *testing.T) {
	f := newFixture(t)

	f.incident(t, "Kanaldan önceki kesinti", -time.Hour, false)
	// Kanal kesintiden SONRA ekleniyor.
	f.channel(t, store.NotificationChannelKindEmail, "nobetci@example.com")

	adapter := &recordingChannel{kind: store.NotificationChannelKindEmail}

	stats, err := f.dispatcher(t, adapter).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 0 || len(adapter.sent) != 0 {
		t.Fatalf("stats = %+v, gönderim = %d — geçmiş bildirildi", stats, len(adapter.sent))
	}
}

// Worker uzun süre kapalı kaldıysa açılışta bildirim çığı olmamalı.
func TestDispatcherIgnoresIncidentsOutsideLookback(t *testing.T) {
	f := newFixture(t)

	// Kanal önce, kesinti sonra — ama ikisi de lookback penceresinin DIŞINDA.
	if _, err := f.pool.Exec(t.Context(),
		`INSERT INTO notification_channels (org_id, kind, name, target, created_at)
		 VALUES ($1, 'email', 'Eski kanal', 'eski@example.com', now() - interval '10 days')`,
		f.orgID); err != nil {
		t.Fatalf("kanal yazılamadı: %v", err)
	}

	f.incident(t, "Çok eski kesinti", -9*24*time.Hour, false)

	adapter := &recordingChannel{kind: store.NotificationChannelKindEmail}

	stats, err := f.dispatcher(t, adapter).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 0 {
		t.Fatalf("pencere dışındaki kesinti kuyruğa alındı: %+v", stats)
	}
}

// Başarısız gönderim yeniden deneniyor, üst sınıra ulaşınca `failed`.
func TestDispatcherRetriesThenGivesUp(t *testing.T) {
	f := newFixture(t)

	f.channel(t, store.NotificationChannelKindEmail, "nobetci@example.com")
	f.incident(t, "Kesinti", 0, false)

	adapter := &recordingChannel{
		kind:     store.NotificationChannelKindEmail,
		failWith: errors.New("sağlayıcı 500 döndü"),
	}

	// Backoff sıfıra yakın: gerçek zamanı beklemeden üç denemeyi ölçmek için.
	dispatcher := NewDispatcher(f.queries, NewRegistryWith(adapter), discardLogger(), DispatcherOptions{
		MaxAttempts: 3,
		Backoff:     time.Nanosecond,
	})

	for range 3 {
		if _, err := dispatcher.RunOnce(t.Context()); err != nil {
			t.Fatalf("beklenmeyen hata: %v", err)
		}
	}

	rows := f.deliveries(t)
	if len(rows) != 1 {
		t.Fatalf("defter satır sayısı = %d", len(rows))
	}

	if rows[0].Status != store.NotificationStatusFailed {
		t.Errorf("durum = %q, beklenen failed", rows[0].Status)
	}

	if rows[0].Attempts != 3 {
		t.Errorf("deneme sayısı = %d, beklenen 3", rows[0].Attempts)
	}
}

// Kanal türü için adapter yoksa gönderim sessizce başarılı sayılmıyor.
func TestDispatcherFailsWhenAdapterMissing(t *testing.T) {
	f := newFixture(t)

	f.channel(t, store.NotificationChannelKindWebhook, "https://ornek.example/hook")
	f.incident(t, "Kesinti", 0, false)

	// Kayıtta YALNIZCA e-posta adapter'ı var.
	dispatcher := f.dispatcher(t, &recordingChannel{kind: store.NotificationChannelKindEmail})

	stats, err := dispatcher.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, beklenen 1 başarısız", stats)
	}
}

// Pasif kanal bildirim almıyor (silmeden susturma).
func TestDispatcherSkipsInactiveChannel(t *testing.T) {
	f := newFixture(t)

	channel := f.channel(t, store.NotificationChannelKindEmail, "nobetci@example.com")

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE notification_channels SET is_active = false WHERE id = $1`, channel.ID); err != nil {
		t.Fatalf("kanal pasifleştirilemedi: %v", err)
	}

	f.incident(t, "Kesinti", 0, false)

	adapter := &recordingChannel{kind: store.NotificationChannelKindEmail}

	stats, err := f.dispatcher(t, adapter).RunOnce(t.Context())
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}

	if stats.Enqueued != 0 || len(adapter.sent) != 0 {
		t.Fatalf("pasif kanala bildirim gitti: %+v", stats)
	}
}
