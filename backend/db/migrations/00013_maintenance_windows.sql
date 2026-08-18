-- +goose Up
-- Planlı bakım penceresi (Faz 4.3).
--
-- İki şeyi birden değiştiriyor ve ikisi de 0.1'de bağlayıcı olarak yazılmıştı:
--   1. Pencere içinde OTOMATİK INCIDENT AÇILMIYOR (planlı kesinti alarm değil).
--   2. Pencere içindeki kontroller uptime'ın PAYDASINDAN düşülüyor ama
--      `maintenance_checks` olarak AYRICA saklanıyor — "o gün planlı bakım
--      vardı" bilgisi kaybolmasın (5.2'nin 90 günlük barları bunu desenle
--      gösterecek).

CREATE TABLE maintenance_windows (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    title       text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    -- Serbest metin, opsiyonel: "ne yapılacak" bilgisi status page'de
    -- ziyaretçiye gösterilecek (5.1).
    description text CHECK (description IS NULL OR char_length(description) BETWEEN 1 AND 2000),

    -- Yarı açık aralık [starts_at, ends_at): ardışık iki pencere aynı anı iki
    -- kez kapsamasın (2.3'ten beri bu projedeki tüm zaman aralıklarının kuralı).
    starts_at timestamptz NOT NULL,
    ends_at   timestamptz NOT NULL,
    CHECK (ends_at > starts_at),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- "Bu organizasyonun pencereleri, zaman sırasına göre" (0.1 §7'nin istediği
-- indeks). Toplulaştırmanın sıcak sorgusu ise monitor üzerinden gidiyor —
-- aşağıdaki bağ tablosunun indeksi.
CREATE INDEX maintenance_windows_org_range_idx
    ON maintenance_windows (org_id, starts_at, ends_at);

-- Bakım ↔ monitor ÇOKTAN-ÇOĞA (0.1 §5): bir bakım genelde birden fazla servisi
-- kapsıyor ("veritabanı bakımı API'yi ve web'i etkiliyor"). "Org geneli" diye
-- ayrı bir bayrak YOK: org'un tamamı için pencere açmak "tüm monitor'leri seç"
-- demek ve iki farklı kapsam kuralı iki farklı sorgu yolu demekti.
CREATE TABLE maintenance_window_monitors (
    window_id  uuid NOT NULL REFERENCES maintenance_windows(id) ON DELETE CASCADE,
    monitor_id uuid NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,

    PRIMARY KEY (window_id, monitor_id)
);

-- SICAK SORGU BU YÖNDE: "şu monitor şu anda bakımda mı" (her DOWN geçişinde)
-- ve "şu kontrol bakım penceresine düşüyor mu" (her toplulaştırma turunda).
-- PK window_id ile başladığı için ters yön kendi indeksini istiyor.
CREATE INDEX maintenance_window_monitors_monitor_idx
    ON maintenance_window_monitors (monitor_id);

-- +goose Down
DROP TABLE IF EXISTS maintenance_window_monitors;
DROP TABLE IF EXISTS maintenance_windows;
