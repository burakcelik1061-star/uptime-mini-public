-- +goose Up
-- Public status page (Faz 5.1) — bu projenin OTURUMSUZ ilk yüzeyi.
--
-- Buraya kadarki her tablo "org-scoped okuma" varsayımıyla yazıldı: kimlik
-- token'dan, organizasyon üyelikten geliyordu. Status page'in ziyaretçisi ise
-- kimliksiz; org'u yalnızca SLUG taşıyor (5.4'te alt alan adı da aynı slug'ı
-- çözecek). Bu yüzden erişim kontrolü bir middleware'de değil, SORGULARIN
-- KENDİSİNDE: aşağıdaki `status_page_items` tablosu "hangi monitor public"
-- sorusunun TEK cevabı ve public sorgular bu tabloyla JOIN'siz çalışamıyor.

CREATE TABLE status_pages (
    id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- ORG BAŞINA TEK SAYFA (0.1 §5) — 1-1 ilişki UNIQUE ile şemada. Çoklu
    -- status page gerçek üründe var ama MVP dışı: 5.4'ün "tek kiracı, tek
    -- slug" varsayımını basit tutuyor.
    org_id uuid NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,

    -- GLOBAL BENZERSİZ, çünkü 5.4'te bu değer bir ALT ALAN ADI olacak
    -- (acme.localhost:3100) ve alt alan adı küresel bir isim alanı.
    --
    -- ORG SLUG'I İLE İLİŞKİSİ (bilinçli karar, docs/5.1-*.md §2):
    -- sayfa açılırken org'un slug'ı KOPYALANIYOR ama sütun AYRI. Aynı sütun
    -- olsaydı organizasyonun adını değiştirmek public URL'i değiştirirdi —
    -- yayımlanmış bir status page adresinin (ve ona verilmiş bağlantıların)
    -- org yönetimindeki bir düzenlemeye bağlı olması kabul edilemez. İki ayrı
    -- isim alanı oldukları için kopyalama anında çakışabilirler; uygulama
    -- (internal/orgs) org slug'ıyla aynı çakışma döngüsünü kullanıyor.
    slug   text NOT NULL UNIQUE
                CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
                CHECK (char_length(slug) BETWEEN 1 AND 63),

    -- YAYIMLANMAMIŞ SAYFA ZİYARETÇİYE GÖRÜNMÜYOR ve VARSAYILAN false:
    -- her organizasyon açılışında bir status page satırı doğuyor (aşağıdaki
    -- backfill + internal/orgs.Create), yani varsayılan true olsaydı bir org
    -- açmak servislerini yayımlamak anlamına gelirdi. Yayımlama açık bir
    -- eylem olmalı.
    is_published boolean NOT NULL DEFAULT false,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- SÜTUN KAPSAMI YİNE DAR (2.1'in kuralı: sütun, YAZMA YOLUNA sahip olan
-- adımda gelir). 0.1'in alan sözlüğünde status page'in ayrıca `title`,
-- `description`, `logo_url` ve `theme_color` alanları da var — hepsi 5.3'ün
-- (özelleştirme) konusu ve yazma yolları orada geliyor. 5.1'de sayfanın
-- başlığı organizasyonun adı; 5.3 onun üzerine yazılabilen bir başlık ekleyecek.

-- Hangi monitor'ler PUBLIC (5.1'in "org seçebilsin" şartı).
--
-- Ayrı bir tablo, `monitors.is_public` bayrağı DEĞİL: 0.1'in modeli
-- (STATUS_PAGE_ITEM) sayfaya ait bir liste tanımlıyor ve 5.3'ün grupları
-- (`group_id`) ile public ad/ sıra alanları buraya sütun olarak gelecek.
-- Bayrak olsaydı o üç alanın da monitor kaydına sızması gerekirdi — monitor
-- "izlenen şey", status page item ise "yayımlanan şey".
CREATE TABLE status_page_items (
    status_page_id uuid NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,

    -- Monitor silinince yayımdan da düşer: silinmiş bir servisi status page'de
    -- göstermeye devam etmek "şu an ne durumda" sorusuna cevabı olmayan bir
    -- satır demekti. (Incident'lar için karar TERSİ, bkz. 00011 — geçmiş
    -- kesinti kaydı kalıcı.)
    monitor_id     uuid NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,

    created_at     timestamptz NOT NULL DEFAULT now(),

    -- Doğal anahtar PK (00011 ve 00010'daki aynı karar): bu satırlara hiçbir
    -- tablo referans vermiyor, yapay bir anahtar yalnızca fazladan bir indeks
    -- olurdu. İstenen benzersizlik zaten bu: aynı monitor sayfaya iki kez
    -- eklenemez.
    PRIMARY KEY (status_page_id, monitor_id)
);

-- Public sorguların SICAK YÖNÜ bu: "şu monitor yayımlanmış mı" (incident ve
-- bakım süzgeçlerinin her satırda sorduğu soru). PK status_page_id ile
-- başladığı için ters yön kendi indeksini istiyor.
CREATE INDEX status_page_items_monitor_idx ON status_page_items (monitor_id);

-- MEVCUT ORGANİZASYONLARA SAYFA ÜRETİMİ.
--
-- 1-1 ilişkiyi "gerektiğinde oluştur" (lazy) yerine burada ve org açılışında
-- kurmak bilinçli: lazy kurulum, sayfanın var olup olmadığını soran her kod
-- yolunda ikinci bir yazma ihtimali demekti (ve iki eşzamanlı istek iki satır
-- denerdi). Slug org slug'ından kopyalanıyor — org slug'ı zaten global
-- benzersiz olduğu için bu toplu ekleme çakışamaz.
INSERT INTO status_pages (org_id, slug)
SELECT o.id, o.slug FROM organizations o;

-- +goose Down
DROP TABLE IF EXISTS status_page_items;
DROP TABLE IF EXISTS status_pages;
