-- +goose Up
-- Status page özelleştirme (Faz 5.3): başlık, açıklama, logo, tema rengi ve
-- servis grupları.
--
-- Bu sütunlar 0.1'in alan sözlüğünde baştan vardı ama 00014'e KONMADI: kural
-- 2.1'den beri aynı — bir sütun, YAZMA YOLUNA sahip olan adımda gelir. 5.1'de
-- sayfanın başlığı organizasyonun adıydı ve onu değiştirecek bir uç yoktu.

ALTER TABLE status_pages
    -- NULL = organizasyonun adı kullanılıyor (5.1'in davranışı korunuyor).
    -- Boş dize ile NULL'ı ayırmak için CHECK: "başlığı sildim" ile "başlığı boş
    -- yaptım" aynı şey olmalı ve uygulama boşu NULL'a çeviriyor.
    ADD COLUMN title       text CHECK (title IS NULL OR char_length(title) BETWEEN 1 AND 120),

    ADD COLUMN description text CHECK (description IS NULL OR char_length(description) BETWEEN 1 AND 500),

    -- Logo DIŞ bir adres (dosya yükleme yok — 0.1 §3'te MVP dışı ve depolama
    -- servisi eklemek "kullanılmayan altyapı taşıma" kararına aykırı olurdu).
    -- Şema yalnızca https'e izin veriyor: http bir logo, https sayfada karışık
    -- içerik uyarısı üretir ve tarayıcı çoğu zaman hiç yüklemez.
    ADD COLUMN logo_url    text CHECK (
        logo_url IS NULL
        OR (logo_url ~ '^https://' AND char_length(logo_url) BETWEEN 1 AND 2048)
    ),

    -- TEMA RENGİ TEK BİR DEĞER ve biçimi şemada zorlanıyor: 6 haneli küçük
    -- harf hex. Serbest metin olsaydı ("red", "rgb(255 0 0)", "javascript:…")
    -- arayüzde ham olarak bir stil özelliğine yazılan değer, denetlenmemiş bir
    -- girdi olurdu. NULL = varsayılan vurgu rengi.
    ADD COLUMN theme_color text CHECK (theme_color IS NULL OR theme_color ~ '^#[0-9a-f]{6}$');

-- Servis grupları ("API", "Web", "Veritabanı").
--
-- Sayfaya ait, organizasyona değil: grup bir SUNUM kararı (0.1'in modeli de
-- STATUS_PAGE_GROUP diyor). Monitor'ün kendi kaydında bir "kategori" alanı
-- olsaydı, aynı monitor'ü farklı sayfalarda farklı gruplamak (6.x/ileri) ve
-- private bir monitor'ü hiç gruplamamak mümkün olmazdı.
CREATE TABLE status_page_groups (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    status_page_id uuid NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,

    name           text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),

    -- Grupların SIRASI kullanıcının kararı: "API" ile "Pazarlama sitesi"nin
    -- hangisinin üstte olacağı alfabeye bırakılamaz. Satır sırası, ayarlar
    -- ekranındaki listenin sırasından geliyor.
    position       integer NOT NULL DEFAULT 0 CHECK (position >= 0),

    created_at     timestamptz NOT NULL DEFAULT now(),

    -- Aynı sayfada iki "API" grubu olamaz: ziyaretçi ikisini ayırt edemezdi.
    UNIQUE (status_page_id, name)
);

CREATE INDEX status_page_groups_page_position_idx
    ON status_page_groups (status_page_id, position, name);

ALTER TABLE status_page_items
    -- NULL = gruplanmamış servis. Grup ZORUNLU DEĞİL: bir org tek bir servis
    -- yayımlıyorsa ondan bir grup adı uydurmasını istemek gereksiz.
    --
    -- ON DELETE SET NULL (CASCADE DEĞİL): bir grubu silmek, içindeki
    -- servisleri YAYIMDAN KALDIRMAK anlamına gelmemeli. Cascade olsaydı
    -- "gruplamayı yeniden düzenliyorum" hareketi sessizce servisleri status
    -- page'den düşürürdü — 5.1'in sızıntı kaygısının ters yönü: yayımda olması
    -- gereken bir servisin kaybolması.
    ADD COLUMN group_id uuid REFERENCES status_page_groups(id) ON DELETE SET NULL;

CREATE INDEX status_page_items_group_idx ON status_page_items (group_id);

-- +goose Down
DROP INDEX IF EXISTS status_page_items_group_idx;
ALTER TABLE status_page_items DROP COLUMN IF EXISTS group_id;
DROP TABLE IF EXISTS status_page_groups;
ALTER TABLE status_pages
    DROP COLUMN IF EXISTS title,
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS logo_url,
    DROP COLUMN IF EXISTS theme_color;
