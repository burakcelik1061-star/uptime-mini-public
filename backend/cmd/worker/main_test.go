package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/worker"
)

// Alt komut ayrıştırması: bilinmeyen bir komut SESSİZCE worker'ı başlatmamalı.
// Aksi hâlde compose'daki healthcheck yazım hatasıyla ("healtcheck") her 10
// saniyede bir ikinci bir worker süreci ayağa kalkardı.
func TestRunRejectsUnknownCommand(t *testing.T) {
	err := run([]string{"healtcheck"})
	if err == nil {
		t.Fatal("bilinmeyen komut kabul edildi")
	}

	if !strings.Contains(err.Error(), "bilinmeyen komut") {
		t.Errorf("hata = %q, 'bilinmeyen komut' bekleniyordu", err)
	}
}

func TestRunRejectsExtraArguments(t *testing.T) {
	if err := run([]string{"healthcheck", "fazladan"}); err == nil {
		t.Fatal("fazladan argüman kabul edildi")
	}
}

// healthcheck alt komutu TAZE bir kalp atışıyla 0 (nil) dönmeli.
func TestRunHealthcheckFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	t.Setenv("WORKER_HEARTBEAT_FILE", path)
	t.Setenv("WORKER_HEARTBEAT_PERIOD", "10s")

	if err := worker.WriteHeartbeat(path, time.Now()); err != nil {
		t.Fatalf("kalp atışı yazılamadı: %v", err)
	}

	if err := run([]string{"healthcheck"}); err != nil {
		t.Fatalf("taze kalp atışı reddedildi: %v", err)
	}
}

// Eskimiş kalp atışı = donmuş worker: konteyner unhealthy görünmeli.
func TestRunHealthcheckStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	t.Setenv("WORKER_HEARTBEAT_FILE", path)
	t.Setenv("WORKER_HEARTBEAT_PERIOD", "1s")

	// Tolerans katsayısı 3 → 1 saniyelik periyotta 3 saniyeden eski olan
	// kalp atışı eskimiş sayılır.
	if err := worker.WriteHeartbeat(path, time.Now().Add(-10*time.Second)); err != nil {
		t.Fatalf("kalp atışı yazılamadı: %v", err)
	}

	if err := run([]string{"healthcheck"}); err == nil {
		t.Fatal("eskimiş kalp atışı kabul edildi")
	}
}

// Süreç hiç başlamadıysa dosya yok — healthcheck bunu da başarısız saymalı.
func TestRunHealthcheckWithoutHeartbeatFile(t *testing.T) {
	t.Setenv("WORKER_HEARTBEAT_FILE", filepath.Join(t.TempDir(), "yok.heartbeat"))

	if err := run([]string{"healthcheck"}); err == nil {
		t.Fatal("olmayan kalp atışı dosyası kabul edildi")
	}
}

// healthcheck VERİTABANI YAPILANDIRMASI GEREKTİRMEMELİ: liveness'ın
// veritabanıyla ilgisi yok (0.3'ün /health ile /ready ayrımı). DATABASE_URL
// boşken de çalışması gerekiyor.
func TestRunHealthcheckDoesNotNeedDatabaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.heartbeat")
	t.Setenv("WORKER_HEARTBEAT_FILE", path)
	t.Setenv("WORKER_HEARTBEAT_PERIOD", "10s")
	t.Setenv("DATABASE_URL", "")

	if err := worker.WriteHeartbeat(path, time.Now()); err != nil {
		t.Fatalf("kalp atışı yazılamadı: %v", err)
	}

	if err := run([]string{"healthcheck"}); err != nil {
		t.Fatalf("healthcheck DATABASE_URL olmadan çalışmadı: %v", err)
	}
}
