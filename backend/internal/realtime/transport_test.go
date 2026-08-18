package realtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Bu dosya 6.1'in ASIL KARARINI gerçek veritabanına karşı ölçüyor: olay
// worker sürecinde `pg_notify` ile yayımlanıyor, api sürecindeki dinleyici onu
// alıp Hub'a veriyor. İki uçta iki AYRI bağlantı var — testte de öyle.

// startListener, dinleyiciyi arka planda başlatır ve test bitiminde durdurur.
func startListener(t *testing.T, hub *realtime.Hub) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)
		realtime.NewListener(testdb.URL(t, "realtime"), hub, testLogger()).Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dinleyici kapanmadı")
		}
	})
}

// publishUntilDelivered, olay abone tarafından görülene kadar yayımlar.
//
// Neden döngü: `NOTIFY` KALICI DEĞİL — dinleyici `LISTEN` ifadesini
// çalıştırmadan önce yayımlanan bir bildirim hiç kimseye ulaşmaz. Sabit bir
// `Sleep` bu yarışı gizlerdi (yavaş bir makinede yine kaybederdi); yeniden
// deneme, bağlantının kurulduğu anı beklemenin belirlenimci yolu.
func publishUntilDelivered(
	t *testing.T, notifier *realtime.Notifier, channel string, event realtime.Event, sub *realtime.Subscription,
) realtime.Event {
	t.Helper()

	deadline := time.After(15 * time.Second)

	for {
		if err := notifier.Publish(t.Context(), channel, event); err != nil {
			t.Fatalf("olay yayımlanamadı: %v", err)
		}

		select {
		case received := <-sub.Events():
			return received
		case <-sub.Dropped():
			t.Fatalf("abonelik düşürüldü: %s", sub.DropReason())
		case <-deadline:
			t.Fatal("olay dinleyiciden geçmedi")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestNotifierDeliversEventThroughPostgres(t *testing.T) {
	pool := testdb.Pool(t, "realtime")

	hub := realtime.NewHub(testLogger())
	startListener(t, hub)

	orgID := uuid.New()
	channel := realtime.OrgChannel(orgID)

	sub := hub.Subscribe(channel)
	defer sub.Close()

	notifier := realtime.NewNotifier(store.New(pool), testLogger())
	event := mustEvent(t, "test.transport", map[string]string{"orgId": orgID.String()})

	received := publishUntilDelivered(t, notifier, channel, event, sub)

	if received.Type != event.Type {
		t.Errorf("Type = %q, beklenen %q", received.Type, event.Type)
	}

	if string(received.Data) != string(event.Data) {
		t.Errorf("Data = %s, beklenen %s", received.Data, event.Data)
	}

	if !received.At.Equal(event.At) {
		t.Errorf("At = %s, beklenen %s", received.At, event.At)
	}
}

// TAŞIMA DA ORG-SCOPED: payload'daki kanal adı hangi abonelerin olayı
// göreceğini belirliyor. Yanlış kanaldaki abone hiçbir şey almamalı.
func TestTransportDoesNotLeakAcrossOrgs(t *testing.T) {
	pool := testdb.Pool(t, "realtime")

	hub := realtime.NewHub(testLogger())
	startListener(t, hub)

	orgA, orgB := uuid.New(), uuid.New()

	subA := hub.Subscribe(realtime.OrgChannel(orgA))
	subB := hub.Subscribe(realtime.OrgChannel(orgB))

	defer subA.Close()
	defer subB.Close()

	notifier := realtime.NewNotifier(store.New(pool), testLogger())

	publishUntilDelivered(t, notifier, realtime.OrgChannel(orgA), mustEvent(t, "sadece-a", nil), subA)

	select {
	case event := <-subB.Events():
		t.Fatalf("B başka org'un olayını aldı: %q", event.Type)
	default:
	}
}

// Bozuk bir payload dinleyiciyi düşürmemeli: NOTIFY kanalına veritabanına
// bağlanabilen HERHANGİ bir oturum yazabilir (bakım sırasında elle atılan bir
// `pg_notify` dahil).
func TestListenerSurvivesMalformedPayload(t *testing.T) {
	pool := testdb.Pool(t, "realtime")

	hub := realtime.NewHub(testLogger())
	startListener(t, hub)

	channel := realtime.OrgChannel(uuid.New())

	sub := hub.Subscribe(channel)
	defer sub.Close()

	notifier := realtime.NewNotifier(store.New(pool), testLogger())

	// Önce dinleyicinin gerçekten bağlandığından emin ol.
	publishUntilDelivered(t, notifier, channel, mustEvent(t, "isinma", nil), sub)

	sendRawNotification(t, pool, "bu json değil")
	sendRawNotification(t, pool, `{"channel":"","event":{}}`)

	// Dinleyici hâlâ ayakta olmalı.
	received := publishUntilDelivered(t, notifier, channel, mustEvent(t, "hala-calisiyor", nil), sub)
	if received.Type != "hala-calisiyor" {
		t.Errorf("Type = %q", received.Type)
	}
}

func TestNotifierRejectsOversizedPayload(t *testing.T) {
	pool := testdb.Pool(t, "realtime")

	notifier := realtime.NewNotifier(store.New(pool), testLogger())

	// Postgres'in NOTIFY sınırı 8000 bayt; sınır uygulamada da duruyor ki
	// hata "sorgu patladı" olarak değil "olay çok büyük" olarak görünsün.
	event := mustEvent(t, "cok-buyuk", map[string]string{"payload": strings.Repeat("x", 9000)})

	err := notifier.Publish(t.Context(), "org:test", event)
	if !errors.Is(err, realtime.ErrPayloadTooLarge) {
		t.Fatalf("hata = %v, beklenen ErrPayloadTooLarge", err)
	}
}

// sendRawNotification, kanala doğrudan (uygulama sözleşmesini atlayarak)
// bildirim yollar.
func sendRawNotification(t *testing.T, pool *pgxpool.Pool, payload string) {
	t.Helper()

	if _, err := pool.Exec(t.Context(), "select pg_notify($1, $2)", realtime.NotifyChannel, payload); err != nil {
		t.Fatalf("ham bildirim gönderilemedi: %v", err)
	}
}
