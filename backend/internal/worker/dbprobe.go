package worker

import (
	"context"
	"log/slog"
	"time"
)

// probeTimeout, tek bir yoklamanın süre sınırı.
//
// Havuzun kendi bağlantı kurma süresinden bağımsız bir üst sınır gerekiyor:
// yoklama takılırsa job döngüsü de takılır ve kalp atışı yazılmaya devam
// ederken worker aslında hiçbir şey yapmıyor olurdu.
const probeTimeout = 5 * time.Second

// Pinger, probe'un veritabanından ihtiyaç duyduğu tek davranış.
// (httpapi'nin `/ready` ucundaki arayüzün aynısı — testler gerçek bir
// veritabanı olmadan da iki yolu ölçebiliyor.)
type Pinger interface {
	Ping(ctx context.Context) error
}

// dbState, yoklamanın gördüğü son durum.
type dbState int

const (
	dbUnknown dbState = iota
	dbReachable
	dbUnreachable
)

// DBProbe, veritabanına erişilebilirliği periyodik olarak yoklar ve YALNIZCA
// DURUM DEĞİŞTİĞİNDE log üretir.
//
// Neden var: worker açılışta veritabanına ulaşamasa bile ölmüyor (0.3'ün
// kararı) — havuz kendini toparlayana kadar ayakta kalıyor. Bu davranışın
// bedeli, veritabanının ne zaman geri geldiğinin görünmemesiydi; artık
// kaybediliş ve geri geliş loga düşüyor.
//
// Neden her turda loglamıyor: 30 saniyede bir "veritabanı erişilebilir"
// satırı, gerçek olayları içinde kaybeden bir gürültü olurdu. Sürecin canlı
// olduğunu kalp atışı dosyası söylüyor, log değil.
type DBProbe struct {
	db     Pinger
	logger *slog.Logger

	// state yalnızca job döngüsünden (tek goroutine) okunup yazılıyor:
	// runtime her job için tek bir döngü çalıştırıyor, koşular üst üste
	// binmiyor.
	state dbState
}

// NewDBProbe, probe'u kurar.
func NewDBProbe(db Pinger, logger *slog.Logger) *DBProbe {
	return &DBProbe{db: db, logger: logger}
}

// Job, probe'u runtime'a kaydedilebilir bir job'a çevirir.
func (p *DBProbe) Job(interval time.Duration) Job {
	return Job{Name: "db-probe", Interval: interval, Run: p.Run}
}

// Run, bir yoklama yapar.
//
// HATA DÖNDÜRMÜYOR (context iptali dışında): "veritabanına ulaşılamıyor" bu
// job'ın BAŞARISIZLIĞI değil, ÖLÇÜMÜ — job'ın işi durumu görmek ve bildirmek.
// Aynı ayrım checker'da da var (hedefin cevap vermemesi bir sonuç, kontrolün
// hiç çalışmaması ayrı bir şey). Ölçümü hata olarak döndürseydik runtime her
// yoklamada "job başarısız" loglar, gerçek durum değişimleri o gürültünün
// içinde kaybolurdu.
func (p *DBProbe) Run(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	err := p.db.Ping(probeCtx)

	// KAPANMA SIRASINDA İPTAL EDİLEN YOKLAMA BİR ÖLÇÜM DEĞİL: durumu
	// "erişilemez"e çevirmek, her worker yeniden başlatmasının sahte bir arıza
	// satırı üretmesi olurdu.
	if err != nil && ctx.Err() != nil {
		return nil
	}

	if err != nil {
		if p.state != dbUnreachable {
			p.logger.Error("veritabanına ulaşılamıyor",
				slog.String("error", err.Error()))
		}

		p.state = dbUnreachable

		return nil
	}

	switch p.state {
	case dbUnreachable:
		p.logger.Info("veritabanı yeniden erişilebilir")
	case dbUnknown:
		p.logger.Info("veritabanı erişilebilir")
	case dbReachable:
		// Durum değişmedi: gürültü üretmiyoruz.
	}

	p.state = dbReachable

	return nil
}
