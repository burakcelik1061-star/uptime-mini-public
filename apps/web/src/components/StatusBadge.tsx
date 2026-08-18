import type { ReactElement } from 'react';

import type { MonitorStatus } from '@/lib/api-types';
import styles from './StatusBadge.module.css';

/**
 * Monitor durumunun görsel karşılığı.
 *
 * KURAL (0.2, kullanıcı şartı): durum ASLA sadece renkle anlatılmaz. Her
 * durumun kendi ŞEKLİ ve METİN etiketi var — renk körlüğü olan bir kullanıcı
 * da, ekran okuyucu kullanan bir kullanıcı da ayırt edebilmeli.
 */

const STATUS_SHAPES: Record<
  MonitorStatus,
  { label: string; className: string; shape: ReactElement }
> = {
  up: {
    label: 'Çalışıyor',
    className: styles.up,
    shape: <circle cx="8" cy="8" r="5" fill="currentColor" />,
  },
  degraded: {
    label: 'Yavaş',
    className: styles.degraded,
    shape: <path d="M8 2.5 14.5 13.5h-13z" fill="currentColor" />,
  },
  down: {
    label: 'Kesinti',
    className: styles.down,
    shape: (
      <path
        d="M4 4l8 8M12 4l-8 8"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
    ),
  },
  paused: {
    label: 'Duraklatıldı',
    className: styles.paused,
    shape: (
      <path
        d="M5.5 3.5v9M10.5 3.5v9"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
    ),
  },
  pending: {
    // Henüz kontrol edilmedi — "veri yok" ile aynı görsel dil (kesikli
    // çember), çünkü ikisi de "bilmiyoruz" demek.
    label: 'Kontrol bekliyor',
    className: styles.pending,
    shape: (
      <circle
        cx="8"
        cy="8"
        r="5"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeDasharray="2 2"
      />
    ),
  },
};

/**
 * BAYAT MONITOR'ÜN ROZETİ (8.6).
 *
 * YENİ BİR GÖRSEL DİL İCAT EDİLMİYOR (kullanıcı kararı): 0.2'nin ZATEN VAR
 * OLAN "veri yok" dili kullanılıyor — kesikli boş çember + `--u-nodata`.
 * `pending` rozeti de aynı şekli taşıyor ve gerekçesi aynı: ikisi de
 * "bilmiyoruz" demek. Değişen tek şey METİN.
 *
 * `status` yine de gösterilmiyor: bayat bir servisin son bilinen durumunu
 * ("Çalışıyor") ziyaretçiye yazmak, tam olarak kapatmaya çalıştığımız
 * körlüğü sürdürmek olurdu.
 */
const STALE_SHAPE = (
  <circle
    cx="8"
    cy="8"
    r="5"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeDasharray="2 2"
  />
);

interface Props {
  status: MonitorStatus;
  /**
   * staleLabel verildiğinde durum yerine "ölçüm yok" rozeti çiziliyor.
   * Metni ÇAĞIRAN üretiyor çünkü süre ("12 dakikadır") ekranın biçimlendirme
   * işi (4.4'ün bölüşümü).
   */
  staleLabel?: string;
}

export function StatusBadge({ status, staleLabel }: Props) {
  const visual = staleLabel
    ? { label: staleLabel, className: styles.stale, shape: STALE_SHAPE }
    : STATUS_SHAPES[status];

  return (
    <span className={`${styles.badge} ${visual.className}`}>
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        aria-hidden="true"
        focusable="false"
      >
        {visual.shape}
      </svg>
      {visual.label}
    </span>
  );
}
