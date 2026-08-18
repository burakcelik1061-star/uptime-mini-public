-- +goose Up
-- Monitor: bu ürünün merkezindeki kayıt — "şu URL'i şu aralıkla kontrol et".
--
-- SÜTUN KAPSAMI BİLİNÇLİ OLARAK DAR (2.1 kararı). 0.1'in alan sözlüğünde
-- monitor'ün ayrıca `next_check_at`, `last_checked_at`, `status_changed_at`,
-- `consecutive_failures/successes`, `failure_threshold`, `recovery_threshold`
-- ve `degraded_response_ms` alanları da var. Bunlar BURADA YOK; kural şu:
-- bir sütun, YAZMA YOLUNA sahip olan adımda gelir.
--
--   * scheduler'ın saati (`next_check_at`) → 3.2, indeksiyle birlikte
--     (`WHERE is_active AND next_check_at <= now()`); bugün yazacak kimse
--     olmadığı için eklenirse anlamı olmayan bir varsayılanla dolar,
--     3.2 geldiğinde yine backfill gerekir.
--   * durum makinesinin sayaçları ve eşikleri → 3.3 (eşiklerin anlamı orada
--     karara bağlanıyor; şimdi eklenen bir `failure_threshold` bugün hiçbir
--     davranışı değiştirmezdi ama API sözleşmesinde "işe yaramaz alan"
--     olarak görünürdü).
--   * `degraded_response_ms` → 2.2 (degraded kararı kontrol anında veriliyor).
--
-- `current_status` İSTİSNA ve burada: monitor listesi ilk günden bir durum
-- göstermek zorunda ve bu adım onu gerçekten YAZIYOR (oluşturmada 'pending',
-- duraklatmada 'paused'). Doğruluk kaynağı yine 3.3'ün durum makinesi olacak.
CREATE TABLE monitors (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Organizasyon silinince monitor'leri de gider: monitor'ün org'suz bir
    -- anlamı yok (incident'lar için aynı şey geçerli DEĞİL, bkz. 0.1 §5).
    org_id     uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    name       text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),

    -- URL biçimi uygulamada ayrıntılı doğrulanıyor (internal/monitors/url.go);
    -- buradaki kısıt son hakem: şemaya http/https dışı bir şey giremesin.
    -- 2048, tarayıcı/proxy'lerin pratikte taşıdığı üst sınır.
    url        text NOT NULL
                    CHECK (url ~ '^https?://')
                    CHECK (char_length(url) BETWEEN 1 AND 2048),

    -- 00001'de tanımlanan enum: GET | HEAD | POST.
    method     monitor_method NOT NULL DEFAULT 'GET',

    -- Alt sınır 30 sn: daha sık kontrol hem hedefe yük bindirir hem de bu
    -- projenin zaman serisi hacim hesabını (0.1 §6) anlamsız kılar.
    -- Üst sınır 24 saat.
    check_interval_seconds integer NOT NULL DEFAULT 60
                    CHECK (check_interval_seconds BETWEEN 30 AND 86400),

    timeout_ms integer NOT NULL DEFAULT 10000
                    CHECK (timeout_ms BETWEEN 1000 AND 60000),

    -- Beklenen HTTP status ARALIĞI (tek değer değil): 200-299 varsayılan,
    -- ama "301 de kabul" veya "401 bekliyorum" (kimlik isteyen bir uç
    -- ayakta demektir) gerçek senaryolar.
    expected_status_min integer NOT NULL DEFAULT 200
                    CHECK (expected_status_min BETWEEN 100 AND 599),
    expected_status_max integer NOT NULL DEFAULT 299
                    CHECK (expected_status_max BETWEEN 100 AND 599),
    CHECK (expected_status_min <= expected_status_max),

    -- Kontrolün kendisi kontrol aralığından uzun süremez: aksi hâlde bir
    -- monitor'ün kontrolleri üst üste binerdi (3.2'nin claim döngüsü bunu
    -- ayrıca ele alacak, ama imkânsız yapılandırmayı şema baştan reddediyor).
    CHECK (timeout_ms <= check_interval_seconds * 1000),

    is_active  boolean NOT NULL DEFAULT true,

    -- Denormalize durum (0.1 §5): pano ve public status page "şu an ne
    -- durumda"yı her istekte ham check satırlarından hesaplayamaz.
    -- İlk kontrol yapılana kadar 'pending'.
    current_status monitor_status NOT NULL DEFAULT 'pending',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Listeleme sorgusunun indeksi: WHERE org_id = $1 ORDER BY created_at.
-- Çok kiracılı bir üründe HER monitor sorgusu org_id ile başlıyor.
CREATE INDEX monitors_org_id_created_at_idx ON monitors (org_id, created_at);

-- Monitor adı/URL'i organizasyon içinde BENZERSİZ DEĞİL: aynı URL'i farklı
-- metot veya farklı beklenen status ile iki kez izlemek meşru bir senaryo
-- (ör. "GET / 200 bekle" + "GET /health 200 bekle" aynı servis için).
-- Benzersizlik dayatmak kullanıcıyı ada takı atmaya zorlardı.

-- +goose Down
DROP TABLE IF EXISTS monitors;
