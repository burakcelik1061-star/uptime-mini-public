-- +goose Up
-- Zaman serisi tablosu: her kontrolün sonucu (Faz 2.3).
--
-- Bu tablo diğerlerinden BİRKAÇ BÜYÜKLÜK MERTEBESİ hızlı büyüyor: 60 sn
-- aralıkta bir monitor günde 1.440 satır, 100 monitor yılda ~52 milyon satır
-- (0.1 §6.1). Şema bu yüzden "sonradan optimize ederiz" mantığıyla değil,
-- büyümesi bilinerek yazıldı.

-- Kontrolü kimin tetiklediği. 0.1'in alan sözlüğünde YOKTU; 2.3'te eklendi,
-- çünkü 2.2'nin manuel "şimdi kontrol et" ucu artık gerçek satır yazıyor:
--   * manuel kontroller DÜZENSİZ aralıklı (kullanıcı üst üste basabilir),
--     uptime yüzdesinin paydasına girerlerse oranı çarpıtırlar (3.4);
--   * durum makinesi (3.3) manuel bir kontrole bakarak monitor'ü DOWN'a
--     çekmemeli — düğmeye basmak kesinti üretmemeli.
-- Ayrım satırda durmazsa bu iki kararın hiçbiri sonradan verilemez.
CREATE TYPE check_trigger AS ENUM ('scheduled', 'manual');

-- PARTITION BY RANGE: budama (3.4) `DELETE` değil `DROP TABLE` olacak —
-- anlık ve ölü satır (bloat) bırakmıyor. 0.1 §6.4'te açık bırakılan karar
-- 0.2'de partition yönünde kapanmıştı; burası uygulaması.
--
-- GÜNLÜK partition: ham veri saklama hedefi 30 gün (0.1 §6.3). Aylık
-- partition'da 30 günlük hedef ancak 30-60 gün arası tutarak sağlanabilirdi;
-- günlük partition'da budama hedefe birebir oturuyor. Bedeli ~37 aktif
-- partition — Postgres'in partition pruning'i için sorun değil.
CREATE TABLE check_results (
    -- BIGINT, uuid DEĞİL (0.1 §7): milyonlarca satırda 8 byte vs 16 byte ve
    -- SIRALI yazımın indeks fragmantasyonuna etkisi burada gerçekten fark
    -- ediyor.
    id              bigint GENERATED ALWAYS AS IDENTITY,

    monitor_id      uuid NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,

    checked_at      timestamptz NOT NULL,

    outcome         check_outcome NOT NULL,

    -- Kaynağı: zamanlanmış kontrol mü, kullanıcının tetiklediği mi.
    triggered_by    check_trigger NOT NULL,

    -- Cevap ALINAMADIYSA (bağlantı/isim/TLS hatası) NULL — 0 uydurulmuyor.
    http_status     integer CHECK (http_status BETWEEN 100 AND 599),

    response_time_ms integer CHECK (response_time_ms >= 0),

    -- Hata tipi ayrı bir enum (0.1 §5): "hepsi hata" olarak toplanmıyor.
    error_type      check_error_type,
    -- Serbest metin, YALNIZCA insan okuması için. Uygulama 300 karakterde
    -- kırpıyor; buradaki sınır son hakem.
    error_message   text CHECK (char_length(error_message) <= 500),

    -- PK'nın partition anahtarını İÇERMESİ Postgres'in kuralı: bölümlenmiş
    -- bir tabloda benzersizlik ancak partition anahtarıyla birlikte
    -- garanti edilebiliyor (her partition kendi indeksini tutuyor).
    PRIMARY KEY (id, checked_at),

    -- TUTARLILIK: sonuç ile hata alanları birbirini tutmalı.
    -- Başarısız kontrolün sebebi HER ZAMAN yazılı olmalı; başarılı/yavaş
    -- kontrolde hata alanı dolu olamaz ve ölçüm eksik kalamaz.
    CONSTRAINT check_results_outcome_consistency CHECK (
        (outcome = 'failure' AND error_type IS NOT NULL)
        OR (
            outcome IN ('success', 'degraded')
            AND error_type IS NULL
            AND error_message IS NULL
            AND http_status IS NOT NULL
            AND response_time_ms IS NOT NULL
        )
    )
) PARTITION BY RANGE (checked_at);

-- 0.1 §7'nin istediği tek indeks: hem "bir monitor'ün son N kontrolü" hem de
-- "şu aralıktaki kontroller" bundan karşılanıyor. DESC yönü, detay
-- sayfasının (2.4) sıcak sorgusu olan "en yeniden geriye" okumasını
-- sıralamasız hâle getiriyor.
--
-- Parent üzerinde tanımlanan indeks TÜM partition'lara otomatik iniyor —
-- yeni partition eklendiğinde indeksi ayrıca oluşturmak gerekmiyor.
CREATE INDEX check_results_monitor_id_checked_at_idx
    ON check_results (monitor_id, checked_at DESC);

-- +goose StatementBegin
-- Partition üreten yardımcı: bugünden geriye 1, ileriye p_days_ahead gün.
--
-- Neden fonksiyon (migration'a elle yazılmış sabit partition listesi değil):
-- sabit liste, depoyu altı ay sonra klonlayan birinde "bugünü" kapsamaz ve
-- İLK KONTROL YAZILAMAZDI. Fonksiyon migration anında çağrıldığı için klon
-- her zaman geçerli bir pencereyle başlıyor; 3.4'ün bakım job'ı aynı
-- fonksiyonu düzenli çağırarak pencereyi ileri kaydıracak.
--
-- DEFAULT partition bilinçli olarak YOK: eksik partition'da INSERT sessizce
-- "default"a düşüp birikeceğine yüksek sesle hata versin (default'ta biriken
-- satırlar ileride o aralık için partition eklemeyi de engelliyor).
CREATE FUNCTION ensure_check_result_partitions(p_days_ahead integer DEFAULT 30)
RETURNS integer
LANGUAGE plpgsql
AS $$
DECLARE
    v_today   date := (now() AT TIME ZONE 'UTC')::date;
    v_day     date;
    v_name    text;
    v_created integer := 0;
BEGIN
    FOR v_day IN
        SELECT d::date
        FROM generate_series(v_today - 1, v_today + p_days_ahead, interval '1 day') AS d
    LOOP
        v_name := 'check_results_' || to_char(v_day, 'YYYYMMDD');

        IF to_regclass(v_name) IS NULL THEN
            -- Sınırlar AÇIKÇA UTC: aralık değerleri oturumun saat dilimine
            -- göre yorumlansaydı, aynı migration farklı makinelerde farklı
            -- sınırlar üretirdi.
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF check_results FOR VALUES FROM (%L) TO (%L)',
                v_name,
                to_char(v_day, 'YYYY-MM-DD') || ' 00:00:00+00',
                to_char(v_day + 1, 'YYYY-MM-DD') || ' 00:00:00+00'
            );
            v_created := v_created + 1;
        END IF;
    END LOOP;

    RETURN v_created;
END;
$$;
-- +goose StatementEnd

-- Açılış penceresi: bugün + 30 gün. Depoyu klonlayan biri bir ay boyunca
-- hiçbir şey yapmadan çalıştırabilir; pencereyi ileri kaydırmak 3.4'ün bakım
-- job'ının işi (aynı fonksiyonu çağıracak).
--
-- Neden 30 ve daha fazlası değil: her partition bir CREATE TABLE, yani
-- migration süresi pencereyle doğru orantılı — 90 günlük pencere test
-- veritabanlarının kurulumunu üç katına çıkarıyordu.
SELECT ensure_check_result_partitions(30);

-- +goose Down
DROP TABLE IF EXISTS check_results;
DROP FUNCTION IF EXISTS ensure_check_result_partitions(integer);
DROP TYPE IF EXISTS check_trigger;
