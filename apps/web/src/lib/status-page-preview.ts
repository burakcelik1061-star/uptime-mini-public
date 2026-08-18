import type { MonitorStatus, PublicStatusPage, ThemeTone } from './api-types';

/**
 * Ayarlar ekranının CANLI ÖNİZLEMESİ için sentetik bir public sayfa (5.3).
 *
 * Neden sunucudan bir "önizleme" ucu istemiyoruz: sayfa yayımlanmamışken
 * public uç 404 dönüyor (ve dönmeli). Önizleme için o kuralı gevşetmek,
 * yayımlanmamış bir sayfayı okunabilir kılan ikinci bir kapı açardı.
 *
 * Önizlemenin sorusu "NASIL GÖRÜNÜYOR", "şu an ne oluyor" değil: kesinti,
 * bakım ve 90 günlük geçmiş taşımıyor. Servislerin durumu ise gerçek —
 * monitor listesi zaten admin'in elinde.
 *
 * SIZINTI RİSKİ YOK: burada üretilen nesne yalnızca ekranda çiziliyor, hiçbir
 * yere gönderilmiyor ve zaten yalnızca admin'in kendi organizasyonunun
 * verisinden besleniyor.
 */

/** Ayarlar formundaki görünüm alanları (boş dize = "yok"). */
export interface Appearance {
  title: string;
  description: string;
  logoUrl: string;
  themeColor: string;
}

interface PreviewService {
  id: string;
  name: string;
  status: MonitorStatus;
  group: string | null;
}

interface PreviewInput {
  slug: string;
  orgName: string;
  appearance: Appearance;
  groups: string[];
  services: PreviewService[];
}

export function previewPage(input: PreviewInput): PublicStatusPage {
  const themeColor = input.appearance.themeColor.trim() || null;

  return {
    slug: input.slug,
    // Sunucudaki kuralın aynısı: özel başlık yoksa organizasyonun adı.
    title: input.appearance.title.trim() || input.orgName,
    description: input.appearance.description.trim() || null,
    logoUrl: input.appearance.logoUrl.trim() || null,
    themeColor,
    themeTone: themeColor ? toneOn(themeColor) : 'dark',
    groups: input.groups,
    overall: overallOf(input.services),
    staleServices: 0,
    services: input.services.map((service) => ({
      id: service.id,
      name: service.name,
      status: service.status,
      statusChangedAt: new Date().toISOString(),
      inMaintenance: false,
      // ÖNİZLEMEDE BAYATLIK YOK (8.6): önizleme ayarların nasıl GÖRÜNECEĞİNİ
      // gösteriyor, canlı ölçüm durumunu değil. Bayatlık kararını sunucu
      // veriyor ve önizleme sunucuya hiç gitmiyor (5.3: yayımlanmamış sayfayı
      // okunabilir kılan ikinci bir kapı açılmadı).
      stale: false,
      staleSince: null,
      // 90 günlük geçmiş önizlemede yok: UptimeBars boş dizide hiçbir şey
      // çizmiyor.
      history: [],
      group: service.group,
    })),
    incidents: [],
    maintenance: [],
  };
}

/**
 * Genel durum özeti — SUNUCUDAKİ `statuspage.Summarize`ın önizleme karşılığı.
 *
 * Bu, projedeki "karar sunucuda" kuralının bilinçli ve DAR bir istisnası:
 * önizlenen sayfa henüz var olmadığı için sunucuya sorulacak bir şey yok.
 * Kural aynı kalıyor — bakım burada hiç yok, `pending`/`paused` "bilmiyoruz".
 */
function overallOf(services: PreviewService[]): PublicStatusPage['overall'] {
  const known = services.filter(
    (service) =>
      service.status === 'up' ||
      service.status === 'degraded' ||
      service.status === 'down',
  );
  const down = known.filter((service) => service.status === 'down');

  if (down.length > 0 && down.length === known.length) return 'major_outage';
  if (down.length > 0) return 'partial_outage';
  if (known.some((service) => service.status === 'degraded')) return 'degraded';
  if (known.length > 0) return 'operational';

  return 'unknown';
}

/**
 * Tema renginin üzerine yazılacak metnin tonu — sunucudaki `statuspage.ToneOn`
 * ile AYNI formül (WCAG bağıl parlaklık, eşik 0.179).
 *
 * İkinci bir kopya olması yalnızca önizleme için: kaydedilmemiş bir renk henüz
 * sunucuda yok. Kaydedildikten sonra tonu SUNUCU söylüyor, bu fonksiyon devre
 * dışı kalıyor.
 */
function toneOn(hexColor: string): ThemeTone {
  const match = /^#([0-9a-f]{6})$/i.exec(hexColor.trim());
  if (!match) return 'light';

  const value = Number.parseInt(match[1], 16);
  const channels = [(value >> 16) & 0xff, (value >> 8) & 0xff, value & 0xff];

  const [red, green, blue] = channels.map((channel) => {
    const ratio = channel / 255;

    return ratio <= 0.04045
      ? ratio / 12.92
      : Math.pow((ratio + 0.055) / 1.055, 2.4);
  });

  const luminance = 0.2126 * red + 0.7152 * green + 0.0722 * blue;

  return luminance > 0.179 ? 'dark' : 'light';
}
