-- +goose Up
-- Incident: bir kesintinin KAYDI (Faz 4.1).
--
-- İki kaynağı var ve ikisi de aynı tabloda yaşıyor:
--   * OTOMATİK — durum makinesi (3.3) bir monitor'ü `down`a geçirdiğinde
--     worker açıyor, monitor geri geldiğinde kapatıyor.
--   * MANUEL — bir admin açıyor (4.4), birden fazla servisi kapsayabiliyor.
--
-- INCIDENT ↔ MONITOR ÇOKTAN-ÇOĞA (0.1 §5): otomatik incident'ta her zaman tek
-- satır olur, ama "veritabanı bakımı API'yi ve web'i düşürdü" gerçek bir
-- senaryo. Bağ ayrı bir tabloda (incident_monitors).

CREATE TABLE incidents (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Organizasyon silinince incident'ları da gider (org'suz bir kesinti
    -- kaydının anlamı yok). MONITOR silinince ise İNCİDENT KALIYOR — bkz.
    -- aşağıdaki auto_monitor_id ve incident_monitors.
    org_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    title  text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),

    -- 00001'de tanımlanan enum'lar.
    -- Timeline'ın son güncellemesi bu sütunu belirliyor (4.2), ama sütun yine
    -- de duruyor: liste sorgusu her incident için son update'i join'lemesin
    -- (0.1 §5'in kararı).
    status incident_status NOT NULL DEFAULT 'investigating',
    impact incident_impact NOT NULL,

    -- OTOMATİK INCIDENT'IN ÖZNESİ: hangi monitor'ün kesintisi bu incident'ı
    -- açtı. Manuel incident'ta NULL.
    --
    -- Neden bu sütun var (asıl sebep bir satır aşağıdaki KISMİ UNIQUE İNDEKS):
    -- "aynı kesinti için ikinci incident açılmasın" garantisi yalnızca
    -- uygulama mantığına bırakılamaz — bu projenin son hakemi veritabanı. İki
    -- worker kopyası (3.1'in tasarımı bunu hedefliyor) aynı anda aynı
    -- monitor'ün DOWN geçişini işlerse, kontrolü uygulamada yapan bir kod iki
    -- incident açardı.
    --
    -- ON DELETE SET NULL: monitor silinse bile incident SİLİNMİYOR (0.1 §7) —
    -- geçmiş kesinti, monitor kaldırıldıktan sonra da status page geçmişinde
    -- kalmalı. `is_auto_created` bu yüzden ayrı bir sütun (auto_monitor_id'den
    -- TÜRETİLMİYOR): monitor silindiğinde özne NULL'a düşer ama kaydın
    -- otomatik doğduğu bilgisi kaybolmamalı.
    auto_monitor_id uuid REFERENCES monitors(id) ON DELETE SET NULL,

    is_auto_created boolean NOT NULL,
    CHECK (auto_monitor_id IS NULL OR is_auto_created),

    -- Kesintinin GERÇEK başlangıcı/bitişi (0.1 §6'nın `downtime_seconds`
    -- yaklaşımının aksine). Otomatik incident'ta bunlar ölçümlerden geliyor:
    -- başlangıç = ardışık hata serisinin İLK başarısız kontrolü, bitiş =
    -- kurtarma serisinin İLK başarılı kontrolü — eşiğin dolduğu an değil
    -- (bkz. db/queries/incidents.sql).
    started_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,

    -- Kapanma İKİ SÜTUNDA birden yazılı; ikisinin ayrışması "çözülmüş ama
    -- açık" gibi anlamsız satırlar demekti.
    CHECK ((status = 'resolved') = (resolved_at IS NOT NULL)),
    CHECK (resolved_at IS NULL OR resolved_at >= started_at),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- "AYNI KESİNTİ İÇİN İKİNCİ INCIDENT AÇILMASIN" — ŞEMADA.
--
-- Kısmi ve unique: bir monitor'ün AÇIK (resolved_at IS NULL) otomatik
-- incident'ı en fazla bir tane olabilir. Kapanmış incident'lar indekse
-- girmiyor, yani aynı monitor yıl içinde yüz kez düşebilir.
--
-- NULL'lar unique indekste birbirine eşit sayılmadığı için manuel
-- incident'lar (auto_monitor_id IS NULL) bu kısıttan etkilenmiyor: bir org
-- aynı anda istediği kadar manuel incident açabilir.
--
-- Uygulama tarafı bu indeksi bir yarış hakemi olarak KULLANIYOR:
-- `INSERT … ON CONFLICT DO NOTHING` (bkz. OpenAutoIncident) — "önce SELECT et,
-- yoksa yaz" deseni iki eş zamanlı worker'a iki incident açtırırdı (1.3'ün
-- slug çakışması ve 3.2'nin claim'iyle aynı disiplin).
CREATE UNIQUE INDEX incidents_open_auto_monitor_idx
    ON incidents (auto_monitor_id)
    WHERE resolved_at IS NULL;

-- Liste sorgusu (4.4): "organizasyonun incident'ları, yeniden eskiye".
CREATE INDEX incidents_org_started_at_idx ON incidents (org_id, started_at DESC);

-- "Açık incident'lar" ayrı bir kısmi indeks (0.1 §7): status page (5.1) ve
-- admin panosu bu sorguyu sık çalıştıracak, kapanmış geçmiş ise sürekli
-- büyüyor.
CREATE INDEX incidents_org_open_idx ON incidents (org_id, started_at DESC)
    WHERE resolved_at IS NULL;

-- Incident'ın etkilediği servisler.
--
-- MONITOR SİLİNİRSE BU SATIR DÜŞER, INCIDENT KALIR (0.1 §7): "3 Mart'ta 40
-- dakika kesinti vardı" bilgisi, o servis üründen kaldırıldı diye
-- silinmemeli. Bedeli: hiçbir monitor'e bağlı olmayan tarihsel incident'lar
-- olabilir — status page onları yine de gösterebilir, çünkü başlık ve
-- timeline kaydın kendisinde duruyor.
CREATE TABLE incident_monitors (
    incident_id uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    monitor_id  uuid NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,

    -- Doğal anahtar PK olarak: 0.1'in ER şemasındaki yapay `id`, hiçbir
    -- tablonun referans vermediği bu satırlarda yalnızca fazladan bir sequence
    -- ve fazladan bir indeks olurdu (3.4'ün rollup tablolarındaki aynı karar).
    -- Asıl istenen benzersizlik zaten bu: aynı monitor bir incident'a iki kez
    -- eklenemez.
    PRIMARY KEY (incident_id, monitor_id)
);

-- "Bu monitor hangi incident'larda geçti" — monitor detay sayfası ve status
-- page için. PK incident_id ile başladığından ters yön kendi indeksini
-- istiyor.
CREATE INDEX incident_monitors_monitor_idx ON incident_monitors (monitor_id);

-- +goose Down
DROP TABLE IF EXISTS incident_monitors;
DROP TABLE IF EXISTS incidents;
