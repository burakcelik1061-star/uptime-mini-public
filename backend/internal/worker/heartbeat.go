package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HeartbeatStaleFactor, canlılık kontrolünün tolerans katsayısı: kalp atışı
// dosyası `period * HeartbeatStaleFactor` süresinden eskiyse worker donmuş
// sayılır.
//
// Neden 1 değil: tek bir gecikmiş yazma (yoğun makine, disk gecikmesi) sağlıklı
// bir süreci "unhealthy" göstermemeli. Neden çok büyük de değil: 3 kat, 10
// saniyelik varsayılan periyotta 30 saniyelik bir tespit süresi demek —
// donduğu fark edilmeyen bir worker'ın anlamı yok.
const HeartbeatStaleFactor = 3

// heartbeatLayout, dosyaya yazılan zaman biçimi. Nanosaniyeli RFC 3339: insan
// da okuyabiliyor (`cat` ile bakılabilir), makine de ayrıştırabiliyor.
const heartbeatLayout = time.RFC3339Nano

// WriteHeartbeat, verilen ana ait zaman damgasını dosyaya yazar.
//
// Yazma ATOMİK (geçici dosya + rename): healthcheck aynı anda okuyorsa yarı
// yazılmış bir dosya görmemeli. Yarı yazılmış dosya "ayrıştırılamayan zaman"
// demek olurdu ve sağlıklı bir worker rastgele unhealthy görünürdü.
func WriteHeartbeat(path string, at time.Time) error {
	dir := filepath.Dir(path)

	temp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("kalp atışı dosyası oluşturulamadı: %w", err)
	}

	tempName := temp.Name()

	if _, err := temp.WriteString(at.UTC().Format(heartbeatLayout) + "\n"); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)

		return fmt.Errorf("kalp atışı yazılamadı: %w", err)
	}

	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)

		return fmt.Errorf("kalp atışı dosyası kapatılamadı: %w", err)
	}

	// Windows'ta rename hedef dosya varsa da çalışıyor (os.Rename bunu
	// MoveFileEx ile hallediyor), yani ayrıca silmek gerekmiyor.
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)

		return fmt.Errorf("kalp atışı dosyası taşınamadı: %w", err)
	}

	return nil
}

// ReadHeartbeat, dosyadaki son kalp atışı zamanını okur.
func ReadHeartbeat(path string) (time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("kalp atışı dosyası okunamadı: %w", err)
	}

	at, err := time.Parse(heartbeatLayout, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, fmt.Errorf("kalp atışı zamanı ayrıştırılamadı: %w", err)
	}

	return at, nil
}

// CheckHeartbeat, worker'ın canlı olup olmadığını söyler: dosya yoksa,
// okunamıyorsa veya `staleAfter`dan eskiyse hata döner.
//
// `worker healthcheck` alt komutunun ve compose healthcheck'inin tek işi bu.
// Bilinçli olarak VERİTABANINA BAKMIYOR — 0.3'ün `/health` (liveness) ile
// `/ready` (readiness) ayrımının aynısı: veritabanı düştüğünde worker'ı
// yeniden başlatmak veritabanını geri getirmez, sadece kesintiyi uzatır.
func CheckHeartbeat(path string, now time.Time, staleAfter time.Duration) error {
	at, err := ReadHeartbeat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("kalp atışı dosyası yok (%s): worker henüz yazmadı", path)
		}

		return err
	}

	if age := now.Sub(at); age > staleAfter {
		return fmt.Errorf("kalp atışı %s önce yazıldı (üst sınır %s): worker takılmış olabilir",
			age.Round(time.Millisecond), staleAfter)
	}

	return nil
}
