-- +goose Up
-- Status page aboneleri (Faz 6.4).
--
-- ZİYARETÇİ İLK KEZ YAZIYOR. 0.1 §2 "buradaki ziyaretçi hiçbir şey yazmıyor"
-- diyordu ve bu, `Subscriber`ın bir `User` OLMAMASININ gerekçesiydi. 6.4 o
-- kuralın BİLİNÇLİ İSTİSNASI: ziyaretçi yalnızca kendi e-posta adresini
-- bırakıyor, ürünün verisine dokunmuyor.
--
-- ABONE BİR KULLANICI DEĞİL (0.1): parolası, oturumu, organizasyon üyeliği
-- yok. Kimliğini taşıyan tek şey e-postasına giden linklerdeki TAHMİN
-- EDİLEMEZ token'lar.

CREATE TYPE subscriber_status AS ENUM ('pending', 'confirmed');

CREATE TABLE status_page_subscribers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    status_page_id uuid NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,

    -- Adres normalleştirilmiş (küçük harf) saklanıyor: 1.1'den beri aynı kural.
    email text NOT NULL CHECK (char_length(email) BETWEEN 3 AND 254),

    status subscriber_status NOT NULL DEFAULT 'pending',

    -- ONAY TOKEN'I HASH'LENMİŞ SAKLANIYOR (1.2'nin refresh token kararının
    -- aynısı): o token adresin SAHİPLİĞİNİ kanıtlıyor ve veritabanını
    -- okuyabilen biri, istediği adresi onaylanmış listeye ekleyebilmemeli.
    -- Ham değer yalnızca e-postadaki linkte var ve tek kullanımlık
    -- (onaylanınca NULL'a iniyor — onaylanmış abonenin bekleyen onayı yoktur).
    confirm_token_hash text,
    confirm_sent_at    timestamptz,

    -- İPTAL TOKEN'I HAM SAKLANIYOR ve bu BİLİNÇLİ BİR AYRIM. Sebebi somut:
    -- gönderilen HER bildirimde çalışan bir iptal linki olmalı (aboneliğin
    -- nasıl biteceğini söylemeyen bir bildirim spam'in tanımı) ve linki
    -- üretmek token'ın ham hâlini gerektiriyor. Hash'lenseydi link yalnızca
    -- onay e-postasında verilebilirdi.
    --
    -- Takas kabul edilebilir çünkü bu token'ın verdiği yetenek YALNIZCA
    -- "bu adresi listeden çıkar". Veritabanını okuyabilen biri zaten tüm
    -- adresleri görüyor; onları listeden çıkarabilmesi buna bir şey eklemiyor.
    -- Onay token'ında aynı takas YAPILMIYOR: orada yetenek "listeye ekle".
    unsubscribe_token text NOT NULL,

    confirmed_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Aynı sayfaya aynı adres iki kez abone olamaz. Bu kısıt aynı zamanda
    -- ENUMERATION savunmasının dayanağı: ikinci istek yeni bir satır değil,
    -- yeni bir ONAY LİNKİ üretiyor ve cevap ilk isteğinkiyle birebir aynı.
    UNIQUE (status_page_id, email),

    -- Onaylanmış abonenin onay zamanı olmalı, onaylanmamışın olmamalı:
    -- "durum" ile "zaman" ayrışmasın.
    CONSTRAINT status_page_subscribers_confirmed_consistency CHECK (
        (status = 'confirmed' AND confirmed_at IS NOT NULL)
        OR (status = 'pending' AND confirmed_at IS NULL)
    )
);

-- Onay ve iptal linkleri token ile geliyor: arama YOLU bu.
CREATE UNIQUE INDEX status_page_subscribers_confirm_token_idx
    ON status_page_subscribers (confirm_token_hash)
    WHERE confirm_token_hash IS NOT NULL;

CREATE UNIQUE INDEX status_page_subscribers_unsubscribe_token_idx
    ON status_page_subscribers (unsubscribe_token);

-- Bildirim gönderilecek aboneler.
CREATE INDEX status_page_subscribers_confirmed_idx
    ON status_page_subscribers (status_page_id)
    WHERE status = 'confirmed';

-- Abone gönderim defteri.
--
-- 6.3'ün `notification_deliveries` tablosuyla AYNI ŞEKİLDE ama AYRI: iki
-- tabloyu tek tabloda birleştirmek, `channel_id` ve `subscriber_id`
-- sütunlarından tam birinin dolu olmasını şart koşan bir CHECK ve iki NULL
-- yabancı anahtar demekti. Ayrı tablolar her satırın gerçek bir yabancı
-- anahtarı olmasını sağlıyor.
CREATE TABLE subscriber_deliveries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    subscriber_id uuid NOT NULL REFERENCES status_page_subscribers(id) ON DELETE CASCADE,
    incident_id   uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    event         notification_event NOT NULL,

    status   notification_status NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),

    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,

    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,

    -- 6.3'ün garantisinin aynısı: aynı aboneye aynı olay iki kez gönderilmez
    -- ve hakem veritabanı.
    UNIQUE (subscriber_id, incident_id, event)
);

CREATE INDEX subscriber_deliveries_pending_idx
    ON subscriber_deliveries (next_attempt_at)
    WHERE status = 'pending';

-- +goose Down
DROP TABLE subscriber_deliveries;
DROP TABLE status_page_subscribers;
DROP TYPE subscriber_status;
