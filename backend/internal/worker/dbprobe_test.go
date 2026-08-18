package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// fakePinger, yoklamanın iki yolunu da (erişilebilir/erişilemez) gerçek bir
// veritabanı olmadan ölçmeyi sağlıyor. Durum değişimi davranışı için gerekli:
// gerçek bir Postgres'i test ortasında düşürüp kaldırmak mümkün değil.
type fakePinger struct {
	err   error
	calls int
}

func (p *fakePinger) Ping(ctx context.Context) error {
	p.calls++

	if err := ctx.Err(); err != nil {
		return err
	}

	return p.err
}

// "Veritabanına ulaşılamıyor" bu job'ın BAŞARISIZLIĞI değil ÖLÇÜMÜ: hata
// döndürseydi runtime her turda "job başarısız" loglar, gerçek durum
// değişimleri o gürültüde kaybolurdu.
func TestDBProbeReportsUnreachableWithoutFailing(t *testing.T) {
	logs := &recorder{}
	pinger := &fakePinger{err: errors.New("bağlantı yok")}
	probe := NewDBProbe(pinger, logs.logger())

	if err := probe.Run(context.Background()); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}

	if !logs.find(slog.LevelError, "veritabanına ulaşılamıyor") {
		t.Error("erişilemezlik loglanmadı")
	}
}

// Durum DEĞİŞMEDİKÇE log üretilmiyor: 30 saniyede bir "erişilebilir" satırı,
// gerçek olayları içinde kaybeden bir gürültü olurdu.
func TestDBProbeLogsOnlyStateChanges(t *testing.T) {
	logs := &recorder{}
	pinger := &fakePinger{}
	probe := NewDBProbe(pinger, logs.logger())

	for range 3 {
		if err := probe.Run(context.Background()); err != nil {
			t.Fatalf("probe hata döndürdü: %v", err)
		}
	}

	if got := countRecords(logs); got != 1 {
		t.Errorf("log satırı = %d, beklenen 1 (yalnızca ilk durum)", got)
	}

	// Erişim kaybı ve geri gelişi: iki yeni satır.
	pinger.err = errors.New("düştü")
	if err := probe.Run(context.Background()); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}
	if err := probe.Run(context.Background()); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}

	pinger.err = nil
	if err := probe.Run(context.Background()); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}

	if got := countRecords(logs); got != 3 {
		t.Errorf("log satırı = %d, beklenen 3 (ilk durum + kayıp + geri geliş)", got)
	}

	if !logs.find(slog.LevelInfo, "yeniden erişilebilir") {
		t.Error("veritabanının geri gelişi loglanmadı")
	}
}

// KAPANMA SIRASINDA iptal edilen yoklama bir ölçüm değil: durumu
// "erişilemez"e çevirmek, her worker yeniden başlatmasının sahte bir arıza
// satırı üretmesi olurdu.
func TestDBProbeIgnoresCancellation(t *testing.T) {
	logs := &recorder{}
	probe := NewDBProbe(&fakePinger{}, logs.logger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := probe.Run(ctx); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}

	if countRecords(logs) != 0 {
		t.Error("kapanma sırasındaki iptal loglandı")
	}
}

// GERÇEK VERİTABANINA karşı: sahte Pinger'ın ölçemediği tek şey, havuzun
// gerçekten yoklanabilir olduğu (pgxpool'un Ping'inin bu job'un beklediği
// arayüzü karşıladığı).
func TestDBProbeAgainstRealDatabase(t *testing.T) {
	pool := testdb.Pool(t, "worker")

	logs := &recorder{}
	probe := NewDBProbe(pool, logs.logger())

	if err := probe.Run(t.Context()); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}

	if !logs.find(slog.LevelInfo, "veritabanı erişilebilir") {
		t.Error("erişilebilirlik loglanmadı")
	}

	// Havuz kapatıldıktan sonra yoklama erişilemezlik bildirmeli — ölümcül
	// olmadan.
	pool.Close()

	if err := probe.Run(t.Context()); err != nil {
		t.Fatalf("probe hata döndürdü: %v", err)
	}

	if !logs.find(slog.LevelError, "veritabanına ulaşılamıyor") {
		t.Error("kapanmış havuz erişilemez olarak loglanmadı")
	}
}

// Job, runtime'a kaydedilebilir olmalı (ad ve aralık doğrulamasından geçsin).
func TestDBProbeJobIsRegisterable(t *testing.T) {
	runtime := New(Options{Logger: discardLogger()})

	if err := runtime.Register(NewDBProbe(&fakePinger{}, discardLogger()).Job(time.Second)); err != nil {
		t.Fatalf("job kaydedilemedi: %v", err)
	}

	if names := runtime.JobNames(); len(names) != 1 || names[0] != "db-probe" {
		t.Errorf("job adları = %v, beklenen [db-probe]", names)
	}
}

func countRecords(logs *recorder) int {
	logs.mu.Lock()
	defer logs.mu.Unlock()

	return len(logs.records)
}
