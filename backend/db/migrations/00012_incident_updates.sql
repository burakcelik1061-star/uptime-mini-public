-- +goose Up
-- Incident timeline: bir kesintinin zaman damgalı güncellemeleri (Faz 4.2).
--
-- Status page'lerin asıl anlattığı şey bu satırlardır: "araştırıyoruz" →
-- "sebebi bulduk" → "izliyoruz" → "çözüldü". `incidents.status` yalnızca son
-- durumu taşır; NE OLDUĞU burada yazılı.
--
-- İki yazar var:
--   * KULLANICI  — bir üye/admin elle yazıyor (author_id dolu)
--   * SİSTEM     — 4.1'in otomatik açma/kapaması (author_id NULL)
-- Otomatik kayıtların da timeline'da görünmesi bilinçli: "bu incident'ı kim
-- açtı, ne zaman kapandı" sorusunun cevabı geçmişin içinde durmalı, yalnızca
-- worker logunda değil.

CREATE TABLE incident_updates (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,

    -- Yazar silinirse satır KALIYOR (kesintinin anlatısı bir kullanıcının
    -- varlığına bağlı olmamalı) ama kim yazdığı kaybolur.
    author_id uuid REFERENCES users(id) ON DELETE SET NULL,

    -- `is_system` author_id'den TÜRETİLMİYOR (4.1'deki `is_auto_created` ile
    -- aynı gerekçe): kullanıcı silindiğinde author_id NULL'a düşüyor ve
    -- türetilmiş bir sütun o satırı sistem yazmış gibi gösterirdi.
    is_system boolean NOT NULL DEFAULT false,
    CHECK (NOT is_system OR author_id IS NULL),

    -- Güncellemenin ilan ettiği durum; incidents.status bunu takip ediyor.
    status incident_status NOT NULL,

    -- Serbest metin. Üst sınır 2000: bir status page girdisi paragraf, makale
    -- değil (ve gövde sınırı zaten 64 KiB, bkz. decodeJSON).
    body text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 2000),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Timeline sorgusu: "şu incident'ın güncellemeleri, kronolojik". `id` sıranın
-- son hakemi — aynı milisaniyeye düşen iki satırın sırası deterministik olsun
-- (2.3'ün `ORDER BY checked_at DESC, id DESC` kararının aynısı).
CREATE INDEX incident_updates_incident_idx
    ON incident_updates (incident_id, created_at, id);

-- +goose Down
DROP TABLE IF EXISTS incident_updates;
