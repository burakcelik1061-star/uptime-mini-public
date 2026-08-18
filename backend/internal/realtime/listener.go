package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listener, Postgres NOTIFY kanalını dinler ve gelen olayları Hub'a verir.
//
// api sürecinde çalışır: worker'da doğan olayların api'ye girdiği TEK kapı.
type Listener struct {
	url    string
	hub    *Hub
	logger *slog.Logger
}

const (
	// listenMinBackoff / listenMaxBackoff, bağlantı koptuğunda yeniden deneme
	// aralığı. Alt sınır kısa (kopma çoğu zaman anlık), üst sınır var ki
	// veritabanı uzun süre yokken saniyede bir bağlanma denemesiyle log
	// dolmasın.
	listenMinBackoff = 500 * time.Millisecond
	listenMaxBackoff = 30 * time.Second

	// listenCloseTimeout, kapanırken bağlantıyı düzgün kapatmak için tanınan
	// süre. Çağıranın context'i o noktada iptalli olduğu için ayrı bir sınır
	// gerekiyor (bkz. scheduler'ın writeContext'i, 3.2).
	listenCloseTimeout = time.Second
)

// NewListener, dinleyiciyi kurar.
//
// HAVUZDAN DEĞİL, KENDİ BAĞLANTISI: `LISTEN` bir OTURUM durumu — bağlantı
// havuza geri verilirse (veya havuz onu MaxConnLifetime dolduğunda tazelerse)
// dinleme sessizce biter. Havuzdan bir bağlantı alıp süresiz tutmak ise
// istek karşılayan havuzu kalıcı olarak bir eksiltmek demekti. İkisi de
// istenmiyor: dinleyicinin kendi bağlantısı var, kendi yeniden bağlanma
// döngüsüyle birlikte.
func NewListener(databaseURL string, hub *Hub, logger *slog.Logger) *Listener {
	if logger == nil {
		logger = slog.Default()
	}

	return &Listener{url: databaseURL, hub: hub, logger: logger}
}

// Run, ctx iptal edilene kadar dinler; bağlantı koparsa yeniden bağlanır.
//
// HATA DÖNDÜRMÜYOR (ctx iptali dışında): veritabanına ulaşamamak bu bileşenin
// başarısızlığı değil ÖLÇÜMÜ — worker'ın `db-probe` job'ıyla aynı ayrım (3.1).
// api canlı yayın olmadan da istek karşılamaya devam etmeli; dönen bir hata
// süreci düşürseydi, geçici bir veritabanı kesintisi tüm API'yi kapatırdı.
func (l *Listener) Run(ctx context.Context) {
	backoff := listenMinBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		err := l.listen(ctx, func() { backoff = listenMinBackoff })

		if ctx.Err() != nil {
			l.logger.Info("canlı yayın dinleyicisi kapandı")

			return
		}

		l.logger.Warn("canlı yayın dinleyicisi koptu, yeniden bağlanılacak",
			slog.String("error", err.Error()),
			slog.Duration("retry_in", backoff))

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, listenMaxBackoff)
	}
}

// listen, tek bir bağlantı ömrü boyunca dinler. Bağlantı kurulduğunda
// onConnected çağrılır (yeniden deneme aralığını sıfırlamak için).
func (l *Listener) listen(ctx context.Context, onConnected func()) error {
	conn, err := pgx.Connect(ctx, l.url)
	if err != nil {
		return fmt.Errorf("dinleyici bağlantısı kurulamadı: %w", err)
	}

	defer func() {
		// Kapanışta ctx zaten iptalli olabilir; bağlantıyı kapatmak için
		// iptalden kopmuş kısa ömürlü bir context gerekiyor.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listenCloseTimeout)
		defer cancel()

		_ = conn.Close(closeCtx)
	}()

	// Kanal adı sabit bir tanımlayıcı ama yine de Sanitize'dan geçiyor: LISTEN
	// parametre almıyor, yani dize birleştirmenin tek güvenli biçimi bu.
	if _, err := conn.Exec(ctx, "listen "+pgx.Identifier{NotifyChannel}.Sanitize()); err != nil {
		return fmt.Errorf("kanal dinlenemedi: %w", err)
	}

	onConnected()
	l.logger.Info("canlı yayın dinleyicisi bağlandı", slog.String("channel", NotifyChannel))

	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("bildirim beklenirken hata: %w", err)
		}

		l.dispatch(notification.Payload)
	}
}

// dispatch, gelen payload'ı çözüp Hub'a verir.
//
// BOZUK PAYLOAD DİNLEYİCİYİ DÜŞÜRMÜYOR: NOTIFY kanalına veritabanına
// bağlanabilen herhangi bir oturum yazabilir (bakım sırasında elle atılan bir
// `pg_notify` dahil). Çözümlenemeyen bir satır yüzünden yayının tamamen
// durması, gürültüye verilmiş ölçüsüz bir tepki olurdu.
func (l *Listener) dispatch(payload string) {
	var env envelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		l.logger.Warn("çözümlenemeyen canlı olay atlandı",
			slog.String("error", err.Error()),
			slog.Int("bytes", len(payload)))

		return
	}

	if env.Channel == "" || env.Event.Type == "" {
		l.logger.Warn("eksik alanlı canlı olay atlandı",
			slog.String("channel", env.Channel),
			slog.String("event", env.Event.Type))

		return
	}

	delivered := l.hub.Publish(env.Channel, env.Event)

	l.logger.Debug("canlı olay dağıtıldı",
		slog.String("channel", env.Channel),
		slog.String("event", env.Event.Type),
		slog.Int("delivered", delivered))
}
