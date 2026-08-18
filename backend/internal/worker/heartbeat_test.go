package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteAndReadHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	at := time.Date(2026, 8, 12, 10, 30, 0, 123_456_789, time.UTC)

	if err := WriteHeartbeat(path, at); err != nil {
		t.Fatalf("yazma başarısız: %v", err)
	}

	got, err := ReadHeartbeat(path)
	if err != nil {
		t.Fatalf("okuma başarısız: %v", err)
	}

	if !got.Equal(at) {
		t.Errorf("okunan zaman = %s, beklenen %s", got, at)
	}
}

// Yazma tekrarlanabilir olmalı: aynı yola ikinci yazma (rename ile) hedefin
// üzerine binmeli. Windows'ta rename'in var olan dosyaya izin vermemesi
// klasik bir tuzak — os.Rename bunu MoveFileEx ile çözüyor, testi burada.
func TestWriteHeartbeatOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	first := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)

	if err := WriteHeartbeat(path, first); err != nil {
		t.Fatalf("ilk yazma başarısız: %v", err)
	}
	if err := WriteHeartbeat(path, second); err != nil {
		t.Fatalf("ikinci yazma başarısız: %v", err)
	}

	got, err := ReadHeartbeat(path)
	if err != nil {
		t.Fatalf("okuma başarısız: %v", err)
	}

	if !got.Equal(second) {
		t.Errorf("okunan zaman = %s, beklenen %s", got, second)
	}

	// Geçici dosya artığı KALMAMALI: her kalp atışında bir dosya bırakmak,
	// günler süren bir süreçte /tmp'i dolduran bir sızıntı olurdu.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("dizin okunamadı: %v", err)
	}

	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		t.Errorf("dizindeki dosyalar = %s, yalnızca kalp atışı bekleniyordu", strings.Join(names, ", "))
	}
}

func TestCheckHeartbeatFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	if err := WriteHeartbeat(path, now.Add(-20*time.Second)); err != nil {
		t.Fatalf("yazma başarısız: %v", err)
	}

	if err := CheckHeartbeat(path, now, 30*time.Second); err != nil {
		t.Errorf("taze kalp atışı reddedildi: %v", err)
	}
}

func TestCheckHeartbeatStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	if err := WriteHeartbeat(path, now.Add(-31*time.Second)); err != nil {
		t.Fatalf("yazma başarısız: %v", err)
	}

	err := CheckHeartbeat(path, now, 30*time.Second)
	if err == nil {
		t.Fatal("eskimiş kalp atışı kabul edildi")
	}

	if !strings.Contains(err.Error(), "takılmış olabilir") {
		t.Errorf("hata mesajı = %q, eskime nedeni bekleniyordu", err)
	}
}

// Dosya HİÇ YOKSA: worker daha açılmadı ya da yazamıyor — ikisi de "canlı
// değil". Mesaj, healthcheck çıktısında yolu göstermeli.
func TestCheckHeartbeatMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yok.heartbeat")

	err := CheckHeartbeat(path, time.Now(), time.Minute)
	if err == nil {
		t.Fatal("olmayan dosya kabul edildi")
	}

	if !strings.Contains(err.Error(), path) {
		t.Errorf("hata mesajı = %q, dosya yolu bekleniyordu", err)
	}
}

// Yarı yazılmış/bozuk dosya sessizce "canlı" sayılmamalı.
func TestCheckHeartbeatCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	if err := os.WriteFile(path, []byte("bu bir zaman değil"), 0o600); err != nil {
		t.Fatalf("dosya yazılamadı: %v", err)
	}

	if err := CheckHeartbeat(path, time.Now(), time.Minute); err == nil {
		t.Fatal("bozuk dosya kabul edildi")
	}
}
