import type { ReactElement } from 'react';

import type { OverallStatus } from '@/lib/api-types';
import styles from './OverallStatusBanner.module.css';

/**
 * Status sayfasının en üstündeki TEK SATIRLIK genel durum (5.1'in açık talebi).
 *
 * KARAR SUNUCUDA (`statuspage.Summarize`), burada yalnızca METNE ÇEVRİLİYOR.
 * Aksi hâlde "tüm sistemler çalışıyor" cümlesinin ikinci bir tanımı doğardı ve
 * 6.2'nin canlı güncellemesi geldiğinde ikisi sessizce ayrışabilirdi.
 *
 * KURAL (0.2): durum asla sadece renkle anlatılmaz — her değerin kendi ŞEKLİ ve
 * METNİ var (StatusBadge ile aynı disiplin).
 */

const VISUALS: Record<
  OverallStatus,
  { label: string; detail: string; className: string; shape: ReactElement }
> = {
  operational: {
    label: 'Tüm sistemler çalışıyor',
    detail: 'Yayımlanan servislerin hepsi yanıt veriyor.',
    className: styles.operational,
    shape: (
      <path
        d="M6 12.5l4 4L18.5 8"
        fill="none"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    ),
  },
  degraded: {
    label: 'Performans düşüklüğü',
    detail: 'Servisler yanıt veriyor ama en az biri beklenenden yavaş.',
    className: styles.degraded,
    shape: <path d="M12 3 22 20H2z" fill="currentColor" />,
  },
  partial_outage: {
    label: 'Kısmi kesinti',
    detail: 'Bazı servisler yanıt vermiyor.',
    className: styles.partial,
    shape: (
      <path
        d="M6 6l12 12M18 6L6 18"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
      />
    ),
  },
  major_outage: {
    label: 'Büyük kesinti',
    detail: 'Yayımlanan servislerin hiçbiri yanıt vermiyor.',
    className: styles.major,
    shape: (
      <>
        <circle cx="12" cy="12" r="9" fill="currentColor" />
        <path d="M6 12h12" stroke="var(--u-bg)" strokeWidth="3" />
      </>
    ),
  },
  maintenance: {
    label: 'Planlı bakım',
    detail: 'En az bir servis planlı bakım penceresinde.',
    className: styles.maintenance,
    shape: (
      <path
        d="M14.5 3.5a5 5 0 0 0-6 6.5L3 15.5 8.5 21l5.5-5.5a5 5 0 0 0 6.5-6l-3.2 3.2-2.8-.7-.7-2.8z"
        fill="currentColor"
      />
    ),
  },
  unknown: {
    label: 'Durum bilinmiyor',
    // "Veri yok" ile "her şey yolunda" ASLA aynı şey değil (0.1'in bağlayıcı
    // kuralının durum tarafındaki karşılığı).
    // 8.6: `unknown`ın artık ÜÇ sebebi var — hiç ölçülmemiş, duraklatılmış,
    // ya da ÖLÇÜMÜ DURMUŞ (bayat). "Henüz ölçüm yok" demek üçüncüsünde
    // yanlıştı: ölçmüştük, artık ölçmüyoruz. Metin sebebe bağlı kalmıyor;
    // hangi servisin neden bilinmediğini satırlar ve sayfa üstündeki
    // bayatlık notu söylüyor.
    detail: 'Yayımlanan servislerin durumu şu anda bilinmiyor.',
    className: styles.unknown,
    shape: (
      <circle
        cx="12"
        cy="12"
        r="8"
        fill="none"
        stroke="currentColor"
        strokeWidth="3"
        strokeDasharray="3 3"
      />
    ),
  },
};

export function OverallStatusBanner({ overall }: { overall: OverallStatus }) {
  const visual = VISUALS[overall];

  return (
    <section
      className={`${styles.banner} ${visual.className}`}
      // Ekran okuyucu için de tek satır: sayfanın ilk cümlesi bu.
      aria-label="Genel durum"
    >
      <svg
        className={styles.icon}
        width="24"
        height="24"
        viewBox="0 0 24 24"
        aria-hidden="true"
        focusable="false"
      >
        {visual.shape}
      </svg>

      <div>
        <p className={styles.label}>{visual.label}</p>
        <p className={styles.detail}>{visual.detail}</p>
      </div>
    </section>
  );
}
