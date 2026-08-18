-- +goose Up
-- Bildirim kanalları ve gönderim defteri (Faz 6.3).
--
-- İKİ TABLO, İKİ AYRI SORU:
--
--   notification_channels    → "bu org nereye haber versin?" (yapılandırma)
--   notification_deliveries  → "şu olay şu kanala GÖNDERİLDİ Mİ?" (durum)
--
-- İkincisi olmadan "iki kez göndermeme" garantisi uygulamada bir bayrağa
-- dayanırdı; burada garanti ŞEMADA (aşağıdaki UNIQUE kısıtı) — 4.1'in kısmi
-- unique indeksiyle aynı disiplin.

CREATE TYPE notification_channel_kind AS ENUM ('email', 'webhook');

CREATE TABLE notification_channels (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    kind notification_channel_kind NOT NULL,

    -- Kullanıcının verdiği ad: aynı türden birden çok kanal ayırt edilebilsin
    -- ("Nöbetçi e-postası", "Slack köprüsü").
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),

    -- HEDEF TEK BİR SÜTUN, tür başına ayrı sütun değil: iki sütun olsaydı
    -- "webhook kanalı ama e-posta sütunu dolu" gibi anlamsız satırlar mümkün
    -- olurdu. Biçim doğrulaması SERVİSTE (internal/notifications) çünkü kural
    -- türe göre değişiyor; şema yalnızca uzunluğu ve boşluğu bağlıyor.
    target text NOT NULL CHECK (char_length(target) BETWEEN 1 AND 2048),

    -- Kanalı SİLMEDEN susturmak (monitor'ün is_active'i ile aynı fikir).
    is_active boolean NOT NULL DEFAULT true,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Aynı org'da aynı hedefi iki kez eklemek = her olayda iki bildirim.
    UNIQUE (org_id, kind, target)
);

CREATE INDEX notification_channels_org_active_idx
    ON notification_channels (org_id)
    WHERE is_active;

-- Hangi olay bildirime konu: 4.1'in otomatik açma/kapamasının iki ucu.
-- Timeline güncellemeleri (4.2) BİLİNÇLİ OLARAK dışarıda — 6.3'ün istediği
-- "açıldığında/kapandığında" ve her ara nota bildirim göndermek, bildirimi
-- kullanılmaz hâle getiren şeyin kendisi.
CREATE TYPE notification_event AS ENUM ('incident_opened', 'incident_resolved');

CREATE TYPE notification_status AS ENUM ('pending', 'sent', 'failed');

CREATE TABLE notification_deliveries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    channel_id  uuid NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    incident_id uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    event       notification_event NOT NULL,

    status   notification_status NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),

    -- Yeniden deneme zamanı: başarısız bir gönderim hemen tekrar denenirse
    -- (ör. sağlayıcı 500 veriyor) tur başına aynı hatayı üretir. Geri çekilme
    -- bu sütunla, ayrı bir kuyruk mekanizması olmadan yapılıyor.
    next_attempt_at timestamptz NOT NULL DEFAULT now(),

    last_error text,

    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,

    -- "İKİ KEZ GÖNDERİLMESİN" GARANTİSİ BURADA, UYGULAMADA DEĞİL.
    -- 4.1'in kısmi unique indeksinin aynı gerekçesi: "önce SELECT et, kayıt
    -- yoksa gönder" iki worker kopyasında iki bildirim üretirdi. Satırı
    -- `ON CONFLICT DO NOTHING` ile yazmak, yarışın hakemini veritabanı yapıyor.
    UNIQUE (channel_id, incident_id, event)
);

-- Sıradaki işi bulan sorgunun indeksi (bkz. db/queries/notifications.sql →
-- ClaimPendingDeliveries). Kısmi: gönderilmiş satırlar — yani zamanla
-- tablonun ezici çoğunluğu — indekste yer tutmuyor.
CREATE INDEX notification_deliveries_pending_idx
    ON notification_deliveries (next_attempt_at)
    WHERE status = 'pending';

-- +goose Down
DROP TABLE notification_deliveries;
DROP TYPE notification_status;
DROP TYPE notification_event;
DROP TABLE notification_channels;
DROP TYPE notification_channel_kind;
